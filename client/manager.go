package client

import (
	"log"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/simpleiot/simpleiot/data"
)

// Manager manages a node type, watches for changes, adds/removes instances that get
// added/deleted
type Manager[T any] struct {
	// initial state
	nc        *nats.Conn
	root      string
	nodeType  string
	construct func(*nats.Conn, T) Client

	// synchronization fields
	stop      chan struct{}
	clientsWG sync.WaitGroup

	// The following state data is protected by the lock Mutex and must be locked
	// before accessing
	lock          sync.Mutex
	nodes         map[string]data.NodeEdge
	clients       map[string]Client
	stopPointSubs map[string]func()
	stopEdgeSubs  map[string]func()
}

// NewManager takes constructor for a node client and returns a Manager for that client
// The Node Type is inferred from the Go type passed in, so you must name Go client
// Types to manage the node type definitions.
func NewManager[T any](nc *nats.Conn, root string,
	construct func(nc *nats.Conn, config T) Client) *Manager[T] {
	var x T
	nodeType := reflect.TypeOf(x).Name()
	nodeType = strings.ToLower(nodeType[0:1]) + nodeType[1:]

	return &Manager[T]{
		nc:        nc,
		root:      root,
		nodeType:  nodeType,
		construct: construct,
		stop:      make(chan struct{}),

		// The keys in the below maps are the concatenation
		// of the parent and node IDs, as we need to have a
		// separate client for each parent/node instance as
		// the edge points, and thus the config could be
		// different
		nodes:         make(map[string]data.NodeEdge),
		clients:       make(map[string]Client),
		stopPointSubs: make(map[string]func()),
		stopEdgeSubs:  make(map[string]func()),
	}
}

// Start node manager. This function looks for children of a certain node type.
// When new nodes are found, the data is decoded into the client type config, and the
// constructor for the node client is called. This call blocks until Stop is called.
func (m *Manager[T]) Start() error {
	err := m.scan()
	if err != nil {
		log.Println("Error scanning for new nodes: ", err)
	}
done:
	for {
		select {
		case <-m.stop:
			break done
		case <-time.After(time.Second * 5):
			err := m.scan()
			if err != nil {
				log.Println("Error scanning for new nodes: ", err)
			}
		}
	}
	return nil
}

// Stop manager. This also stops all registered clients and causes Start to exit.
func (m *Manager[T]) Stop(err error) {
	m.lock.Lock()
	for _, c := range m.stopPointSubs {
		c()
	}

	for _, c := range m.stopEdgeSubs {
		c()
	}

	for _, c := range m.clients {
		c.Stop(err)
	}
	m.lock.Unlock()

	clientsDone := make(chan struct{})
	go func() {
		m.clientsWG.Wait()
		close(clientsDone)
	}()

	select {
	case <-clientsDone:
		// all is well
	case <-time.After(time.Second * 5):
		log.Println("BUG: Not all clients shutdown!")
	}

	close(m.stop)
}

func (m *Manager[T]) scan() error {
	children, err := GetNodeChildren(m.nc, m.root, m.nodeType, false, false)

	if err != nil {
		return err
	}

	if len(children) < 0 {
		return nil
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	found := make(map[string]bool)

	// create new nodes
	for _, n := range children {
		mapKey := n.Parent + n.ID
		found[mapKey] = true

		if _, ok := m.nodes[mapKey]; ok {
			continue
		}

		m.nodes[mapKey] = n

		var config T

		err := data.Decode(n, &config)
		if err != nil {
			log.Println("Error decoding node: ", err)
			continue
		}

		client := m.construct(m.nc, config)
		m.clients[mapKey] = client

		func(client Client) {
			stopNodeSub, err := SubscribePoints(m.nc, n.ID, func(points []data.Point) {
				client.Points(points)
			})
			if err != nil {
				log.Println("client manager sub error: ", err)
				return
			}
			m.stopPointSubs[mapKey] = stopNodeSub

			stopEdgeSub, err := SubscribeEdgePoints(m.nc, n.ID, n.Parent, func(points []data.Point) {
				client.EdgePoints(points)
			})
			if err != nil {
				log.Println("client manager edge sub error: ", err)
				return
			}
			m.stopEdgeSubs[mapKey] = stopEdgeSub
		}(client)

		go func(client Client) {
			m.clientsWG.Add(1)
			err := client.Start()
			if err != nil {
				log.Println("Node client returned error: ", err)
			}
			m.clientsWG.Done()
		}(client)
	}

	// remove nodes that have been deleted
	for key, client := range m.clients {
		if _, ok := found[key]; ok {
			continue
		}

		// bus was deleted so close and clear it
		log.Println("removing node: ", m.nodes[key].ID)
		m.stopPointSubs[key]()
		m.stopEdgeSubs[key]()
		client.Stop(nil)
		delete(m.nodes, key)
		delete(m.clients, key)
		delete(m.stopPointSubs, key)
		delete(m.stopEdgeSubs, key)
	}

	return nil
}