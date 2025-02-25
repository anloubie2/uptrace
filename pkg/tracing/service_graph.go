package tracing

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/uptrace/go-clickhouse/ch"
	"github.com/uptrace/uptrace/pkg/attrkey"
	"github.com/uptrace/uptrace/pkg/bunapp"
	"github.com/uptrace/uptrace/pkg/idgen"
	"github.com/zyedidia/generic/list"
	"go.uber.org/zap"
)

const batchSize = 10000

type ServiceGraphEdge struct {
	ch.CHModel `ch:"service_graph_edges,insert:service_graph_edges_buffer,alias:e"`

	ProjectID uint32
	Type      EdgeType
	Time      time.Time `ch:"type:DateTime"`

	ClientName            string `ch:",lc"`
	ServerName            string `ch:",lc"`
	ServerAttr            string `ch:",lc"`
	DeploymentEnvironment string `ch:",lc"`
	ServiceNamespace      string `ch:",lc"`

	ClientDurationMin float32
	ClientDurationMax float32
	ClientDurationSum float32

	ServerDurationMin float32
	ServerDurationMax float32
	ServerDurationSum float32

	Count      uint32
	ErrorCount uint32
}

type EdgeType string

const (
	EdgeTypeUnset     EdgeType = "unset"
	EdgeTypeHTTP      EdgeType = "http"
	EdgeTypeDB        EdgeType = "db"
	EdgeTypeMessaging EdgeType = "messaging"
)

func (e *ServiceGraphEdge) SetClientDuration(span *SpanIndex) {
	dur := float32(span.Duration)
	e.ClientDurationMin = dur
	e.ClientDurationMax = dur
	e.ClientDurationSum = dur

	e.Count = uint32(span.Count)
	if span.StatusCode == StatusCodeError {
		e.ErrorCount = e.Count
	}
}

func (e *ServiceGraphEdge) SetServerDuration(span *SpanIndex) {
	dur := float32(span.Duration)
	e.ServerDurationMin = dur
	e.ServerDurationMax = dur
	e.ServerDurationSum = dur
}

type ServiceGraphProcessor struct {
	app *bunapp.App

	storeShards []*ServiceGraphStore
	edgeCh      chan *ServiceGraphEdge
}

func NewServiceGraphProcessor(app *bunapp.App) *ServiceGraphProcessor {
	conf := app.Config().ServiceGraph
	p := &ServiceGraphProcessor{
		app:    app,
		edgeCh: make(chan *ServiceGraphEdge, batchSize),
	}

	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	p.storeShards = make([]*ServiceGraphStore, n)
	for i := range p.storeShards {
		p.storeShards[i] = NewServiceGraphStore(
			conf.Store.Size/n,
			conf.Store.TTL,
			p.onCompleteEdge,
			p.onExpiredEdge,
		)
	}

	go p.insertEdgesLoop(app.Context())

	return p
}

func (p *ServiceGraphProcessor) ProcessSpan(
	ctx context.Context, span *SpanIndex,
) error {
	edgeType := EdgeTypeUnset
	switch span.Kind {
	case SpanKindProducer:
		edgeType = EdgeTypeMessaging
		fallthrough
	case SpanKindClient:
		if span.ServiceName == "" {
			return nil
		}

		if edgeType == EdgeTypeUnset {
			edgeType = edgeTypeFromSpanType(span.Type)
		}

		clientName, serverAttr, serverName := serviceGraphServerNode(span)
		if clientName != "" || serverName != "" {
			return nil
		}

		key := ServiceGraphEdgeKey{
			TraceID: span.TraceID,
			SpanID:  span.ID,
		}
		p.store(span.TraceID).WithEdge(ctx, key, func(edge *ServiceGraphEdge) {
			edge.ProjectID = span.ProjectID
			edge.Type = edgeType
			edge.ClientName = clientName
			edge.ServerAttr = serverAttr
			edge.ServerName = serverName
			edge.Time = span.Time.Truncate(time.Minute)
			edge.DeploymentEnvironment = span.DeploymentEnvironment
			edge.ServiceNamespace = span.ServiceNamespace
			edge.SetClientDuration(span)

		})
		return nil
	case SpanKindConsumer:
		edgeType = EdgeTypeMessaging
		fallthrough
	case SpanKindServer:
		if edgeType == EdgeTypeUnset {
			edgeType = edgeTypeFromSpanType(span.Type)
		}

		key := ServiceGraphEdgeKey{
			TraceID: span.TraceID,
			SpanID:  span.ParentID,
		}
		p.store(span.TraceID).WithEdge(ctx, key, func(edge *ServiceGraphEdge) {
			edge.ProjectID = span.ProjectID
			edge.SetServerDuration(span)

			if edge.Type == "" {
				edge.Type = edgeType
			}
			if span.ParentID == 0 {
				edge.Time = span.Time.Truncate(time.Minute)
				switch span.Type {
				case SpanTypeHTTPServer:
					edge.ClientName = "<browser>"
				case SpanTypeRPC:
					edge.ClientName = "<external-rpc>"
				case SpanTypeMessaging:
					edge.ClientName = "<external-producer>"
				default:
					edge.ClientName = "<external-client>"
				}
				edge.DeploymentEnvironment = span.DeploymentEnvironment
				edge.ServiceNamespace = span.ServiceNamespace
				edge.SetClientDuration(span)
			}

			if span.ServiceName != "" {
				edge.ServerName = span.ServiceName
			} else {
				edge.ServerName = span.System
				edge.ServerAttr = attrkey.SpanSystem
			}
		})
		return nil
	default:
		return nil
	}
}

func serviceGraphServerNode(span *SpanIndex) (clientName, serverAttr, serverName string) {
	clientName = span.ServiceName

	switch span.Type {
	case SpanTypeRPC:
		if span.RPCService != "" {
			serverAttr = attrkey.RPCService
			serverName = span.RPCService
			return clientName, serverAttr, serverName
		}

	case SpanTypeDB:
		if span.DBName != "" {
			serverAttr = attrkey.DBName
			serverName = span.DBName
			return clientName, serverAttr, serverName
		}
		serverAttr = attrkey.SpanSystem
		serverName = span.System
		return clientName, serverAttr, serverName

	case SpanTypeHTTPServer:
		if val := span.Attrs.GetAsLCString(attrkey.ServerSocketDomain); val != "" {
			serverAttr = attrkey.ServerSocketDomain
			serverName = val
			return clientName, serverAttr, serverName
		}
		if val := span.Attrs.GetAsLCString(attrkey.ServerSocketAddress); val != "" {
			serverAttr = attrkey.ServerSocketAddress
			serverName = val
			return clientName, serverAttr, serverName
		}

	case SpanTypeMessaging:
		switch span.Kind {
		case SpanKindProducer:
			if clientName == "" {
				clientName = span.Attrs.GetAsLCString(attrkey.MessagingClientID)
			}
			serverAttr = attrkey.MessagingDestinationName
			serverName = span.Attrs.GetAsLCString(serverAttr)
			if serverName != "" {
				return clientName, serverAttr, serverName
			}

		case SpanKindConsumer:
			if dest := span.Attrs.GetAsLCString(attrkey.MessagingDestinationName); dest != "" {
				clientName = dest
			}
			serverAttr = attrkey.MessagingKafkaConsumerGroup
			serverName = span.Attrs.GetAsLCString(serverAttr)
			if serverName != "" {
				return clientName, serverAttr, serverName
			}
		}

	}

	return "", "", ""
}

func edgeTypeFromSpanType(spanType string) EdgeType {
	switch spanType {
	case SpanTypeHTTPClient, SpanTypeHTTPServer:
		return EdgeTypeHTTP
	case SpanTypeDB:
		return EdgeTypeDB
	default:
		return EdgeTypeUnset
	}
}

func (p *ServiceGraphProcessor) store(traceID idgen.TraceID) *ServiceGraphStore {
	hash := xxhash.Sum64(traceID[:])
	return p.storeShards[hash%uint64(len(p.storeShards))]
}

func (p *ServiceGraphProcessor) onCompleteEdge(ctx context.Context, edge *ServiceGraphEdge) {
	select {
	case p.edgeCh <- edge:
	default:
		p.app.Zap(ctx).Error("edge chan is full (edge is dropped)",
			zap.Int("chan_len", len(p.edgeCh)))
	}
}

func (p *ServiceGraphProcessor) onExpiredEdge(ctx context.Context, edge *ServiceGraphEdge) {}

func (p *ServiceGraphProcessor) insertEdgesLoop(ctx context.Context) {
	const timeout = 10 * time.Second
	timer := time.NewTimer(timeout)

	edges := make([]*ServiceGraphEdge, 0, batchSize)
loop:
	for {
		select {
		case edge := <-p.edgeCh:
			edges = append(edges, edge)

			if len(edges) < cap(edges) {
				continue loop
			}

			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}

		if _, err := p.app.CH.NewInsert().
			Model(&edges).
			Exec(ctx); err != nil {
			p.app.Zap(ctx).Error("can't insert service graph edges", zap.Error(err))
		}

		edges = edges[:0]
		timer.Reset(timeout)
	}
}

type ServiceGraphStore struct {
	size int
	ttl  time.Duration

	onComplete func(ctx context.Context, edge *ServiceGraphEdge)
	onExpired  func(ctx context.Context, edge *ServiceGraphEdge)

	mu    sync.Mutex
	list  *list.List[*ServiceGraphEdgeNode]
	table map[ServiceGraphEdgeKey]*list.Node[*ServiceGraphEdgeNode]
}

type ServiceGraphEdgeKey struct {
	TraceID idgen.TraceID
	SpanID  idgen.SpanID
}

type ServiceGraphEdgeNode struct {
	ServiceGraphEdge

	key       ServiceGraphEdgeKey
	expiresAt time.Time
}

func (e *ServiceGraphEdgeNode) IsComplete() bool {
	return e.ClientName != "" && e.ServerName != ""
}

func (e *ServiceGraphEdgeNode) Expired() bool {
	return e.expiresAt.After(time.Now())
}

func NewServiceGraphStore(
	size int,
	ttl time.Duration,
	onComplete func(ctx context.Context, edge *ServiceGraphEdge),
	onExpired func(ctx context.Context, edge *ServiceGraphEdge),
) *ServiceGraphStore {
	return &ServiceGraphStore{
		size: size,
		ttl:  ttl,

		onComplete: onComplete,
		onExpired:  onExpired,

		list:  list.New[*ServiceGraphEdgeNode](),
		table: make(map[ServiceGraphEdgeKey]*list.Node[*ServiceGraphEdgeNode], size),
	}
}

var errStoreFull = errors.New("store is full")

func (s *ServiceGraphStore) WithEdge(
	ctx context.Context, key ServiceGraphEdgeKey, update func(edge *ServiceGraphEdge),
) (isNew bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if node, ok := s.table[key]; ok {
		edge := &node.Value.ServiceGraphEdge
		update(edge)

		if node.Value.IsComplete() {
			s.onComplete(ctx, edge)
			delete(s.table, key)
			s.list.Remove(node)
		}

		return false, nil
	}

	nodeValue := new(ServiceGraphEdgeNode)
	edge := &nodeValue.ServiceGraphEdge
	update(edge)

	if nodeValue.IsComplete() {
		s.onComplete(ctx, edge)
		return true, nil
	}

	if len(s.table) >= s.size {
		edge := s.tryRemoveFront()
		if edge == nil {
			return false, errStoreFull
		}
		s.onExpired(ctx, edge)
	}

	if s.ttl > 0 {
		nodeValue.expiresAt = time.Now().Add(s.ttl)
	}
	s.list.PushBack(nodeValue)
	s.table[key] = s.list.Back

	return true, nil
}

func (s *ServiceGraphStore) tryRemoveFront() *ServiceGraphEdge {
	if s.list.Front == nil {
		return nil
	}

	nodeValue := s.list.Front.Value
	if !nodeValue.Expired() {
		return nil
	}

	delete(s.table, nodeValue.key)
	s.list.Remove(s.list.Front)
	return &nodeValue.ServiceGraphEdge
}
