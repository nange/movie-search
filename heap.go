package easypool

import (
	"container/heap"
	"errors"
	"log"
	"net"
	"sync"
	"time"
)

type PriorityQueue []*PoolConn

func (pq PriorityQueue) Len() int { return len(pq) }

func (pq PriorityQueue) Less(i, j int) bool {
	// we want to get the oldest item
	return pq[i].updatedtime.Sub(pq[j].updatedtime) < 0
}

func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
}

func (pq *PriorityQueue) Push(x interface{}) {
	pc := x.(*PoolConn)
	*pq = append(*pq, pc)
}

func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[0 : n-1]
	return item
}

type heapPool struct {
	mu          sync.Mutex
	freeConn    *PriorityQueue
	initialCap  int
	maxCap      int
	maxIdle     int
	idletime    time.Duration
	maxLifetime time.Duration
	cleanerCh   chan struct{}

	factory func() (net.Conn, error)
}

func NewHeapPool(config *PoolConfig) (Pool, error) {
	if config.InitialCap > config.MaxCap || config.Factory == nil {
		return nil, ErrConfigInvalid
	}

	initialCap := 0
	if config.InitialCap > 0 {
		initialCap = config.InitialCap
	}
	maxCap := 20
	if config.MaxCap > 0 {
		maxCap = config.MaxCap
	}
	maxIdle := 5
	if config.MaxIdle > 0 {
		maxIdle = config.MaxIdle
	}
	idletime := 2 * time.Minute
	if config.Idletime > 0 {
		idletime = config.Idletime
	}
	maxLifetime := 15 * time.Minute
	if config.MaxLifetime > 0 {
		maxLifetime = config.MaxLifetime
	}

	hp := &heapPool{
		initialCap:  initialCap,
		maxCap:      maxCap,
		maxIdle:     maxIdle,
		idletime:    idletime,
		maxLifetime: maxLifetime,
		cleanerCh:   make(chan struct{}, 1),
		factory:     config.Factory,
	}

	pq := make(PriorityQueue, 0, maxCap)
	heap.Init(&pq)
	hp.freeConn = &pq
	for i := 0; i < initialCap; i++ {
		conn, err := hp.factory()
		if err != nil {
			return nil, err
		}
		heap.Push(hp.freeConn, hp.wrapConn(conn))
	}

	go hp.cleaner()

	return hp, nil
}

func (hp *heapPool) Get() (net.Conn, error) {
	if hp.freeConn == nil {
		return nil, ErrClosed
	}

	hp.mu.Lock()
	for hp.freeConn.Len() > 0 {
		pc := heap.Pop(hp.freeConn).(*PoolConn)
		if time.Now().Sub(pc.updatedtime) <= hp.maxLifetime {
			hp.mu.Unlock()
			return pc, nil
		}
	}
	hp.mu.Unlock()

	conn, err := hp.factory()
	if err != nil {
		return nil, err
	}
	return hp.wrapConn(conn), nil
}

func (hp *heapPool) Close() {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	hp.cleanerCh <- struct{}{}
	hp.factory = nil
	for hp.freeConn.Len() > 0 {
		pc := heap.Pop(hp.freeConn).(*PoolConn)
		pc.hp = nil
		pc.close()
	}
	hp.freeConn = nil
}

func (hp *heapPool) put(conn *PoolConn) error {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	if hp.freeConn.Len() >= hp.maxCap {
		return errors.New("pool have been filled")
	}
	heap.Push(hp.freeConn, conn)
	return nil
}

func (hp *heapPool) Len() int {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	return hp.freeConn.Len()
}

func (hp *heapPool) cleaner() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			hp.mu.Lock()

			for hp.freeConn.Len() > 0 {
				pc := (*hp.freeConn)[0]
				interval := time.Now().Sub(pc.updatedtime)
				if interval >= hp.maxLifetime {
					heap.Pop(hp.freeConn).(*PoolConn).close()
					continue
				}
				if interval >= hp.idletime && hp.freeConn.Len() > hp.maxIdle {
					heap.Pop(hp.freeConn).(*PoolConn).close()
					continue
				}
				break
			}

			hp.mu.Unlock()

		case <-hp.cleanerCh:
			log.Println("cleaner exited...")
			return
		}
	}
}

func (hp *heapPool) wrapConn(conn net.Conn) net.Conn {
	p := &PoolConn{hp: hp, updatedtime: time.Now()}
	p.Conn = conn
	return p
}