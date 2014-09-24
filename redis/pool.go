// Copyright 2012 Gary Burd
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package redis

import (
	"bytes"
	"container/list"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

var nowFunc = time.Now // for testing

// ErrPoolExhausted is returned from a pool connection method (Do, Send,
// Receive, Flush, Err) when the maximum number of database connections in the
// pool has been reached.
var ErrPoolExhausted = errors.New("redigo: connection pool exhausted")

var (
	errPoolClosed = errors.New("redigo: connection pool closed")
	errConnClosed = errors.New("redigo: connection closed")
)

// Pool maintains a pool of connections. The application calls the Get method
// to get a connection from the pool and the connection's Close method to
// return the connection's resources to the pool.
//
// The following example shows how to use a pool in a web application. The
// application creates a pool at application startup and makes it available to
// request handlers, possibly using a global variable.
//
//  func newPool(server, password string) *redis.Pool {
//      return &redis.Pool{
//          MaxIdle: 3,
//          IdleTimeout: 240 * time.Second,
//          Dial: func () (redis.Conn, error) {
//              c, err := redis.Dial("tcp", server)
//              if err != nil {
//                  return nil, err
//              }
//              if _, err := c.Do("AUTH", password); err != nil {
//                  c.Close()
//                  return nil, err
//              }
//              return c, err
//          },
//          TestOnBorrow: func(c redis.Conn, t time.Time) error {
//              _, err := c.Do("PING")
//              return err
//          },
//      }
//  }
//
//  var (
//      pool *redis.Pool
//      redisServer = flag.String("redisServer", ":6379", "")
//      redisPassword = flag.String("redisPassword", "", "")
//  )
//
//  func main() {
//      flag.Parse()
//      pool = newPool(*redisServer, *redisPassword)
//      ...
//  }
// A request handler gets a connection from the pool and closes the connection
// when the handler is done:
//
//  func serveHome(w http.ResponseWriter, r *http.Request) {
//      conn := pool.Get()
//      defer conn.Close()
//      ....
//  }
//
type Pool struct {

	// Dial is an application supplied function for creating and configuring a
	// connection
	Dial func() (Conn, error)

	// TestOnBorrow is an optional application supplied function for checking
	// the health of an idle connection before the connection is used again by
	// the application. Argument t is the time that the connection was returned
	// to the pool. If the function returns an error, then the connection is
	// closed.
	TestOnBorrow func(c Conn, t time.Time) error

	// Maximum number of idle connections in the pool.
	MaxIdle int

	// Maximum number of connections allocated by the pool at a given time.
	// When zero, there is no limit on the number of connections in the pool.
	MaxActive int

	// Close connections after remaining idle for this duration. If the value
	// is zero, then idle connections are not closed. Applications should set
	// the timeout to a value less than the server's timeout.
	IdleTimeout time.Duration

	// mu protects fields defined below.
	mu     sync.Mutex
	closed bool
	active int

	// Stack of idleConn with most recently used at the front.
	idle list.List

	// Max Timeout waiting for Pool
	MaxActiveTimeout time.Duration
}

type idleConn struct {
	c Conn
	t time.Time
}

// NewPool creates a new pool. This function is deprecated. Applications should
// initialize the Pool fields directly as shown in example.
func NewPool(newFn func() (Conn, error), maxIdle int) *Pool {
	return &Pool{Dial: newFn, MaxIdle: maxIdle, MaxActiveTimeout: 30 * time.Second}
}

// Get gets a connection. The application must close the returned connection.
// This method always returns a valid connection so that applications can defer
// error handling to the first use of the connection. If there is an error
// getting an underlying connection, then the connection Err, Do, Send, Flush
// and Receive methods return that error.
func (p *Pool) Get() Conn {
	c, err := p.get()
	if err != nil {
		return errorConnection{err}
	}
	return &pooledConnection{p: p, c: c}
}

// ActiveCount returns the number of active connections in the pool.
func (p *Pool) ActiveCount() int {
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	return active
}

// Close releases the resources used by the pool.
func (p *Pool) Close() error {
	p.mu.Lock()
	idle := p.idle
	p.idle.Init()
	p.closed = true
	p.active -= idle.Len()
	p.mu.Unlock()
	for e := idle.Front(); e != nil; e = e.Next() {
		e.Value.(idleConn).c.Close()
	}
	return nil
}

// get prunes stale connections and returns a connection from the idle list or
// creates a new connection.
func (p *Pool) get() (Conn, error) {
	p.mu.Lock()

	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("redigo: get on closed pool")
	}

	// Prune stale connections.

	if timeout := p.IdleTimeout; timeout > 0 {
		for i, n := 0, p.idle.Len(); i < n; i++ {
			e := p.idle.Back()
			if e == nil {
				break
			}
			ic := e.Value.(idleConn)
			if ic.t.Add(timeout).After(nowFunc()) {
				break
			}
			p.idle.Remove(e)
			p.active -= 1
			p.mu.Unlock()
			ic.c.Close()
			p.mu.Lock()
		}
	}

	// if no idle connections, wait for one to be available
	if p.MaxActive > 0 && p.active >= p.MaxActive {
		p.mu.Unlock()
		for t := time.Duration(0); t < p.MaxActiveTimeout; t = t + 50*time.Millisecond {
			time.Sleep(50 * time.Millisecond)
		}

		p.mu.Lock()
	}
	// Get idle connection.

	for i, n := 0, p.idle.Len(); i < n; i++ {
		e := p.idle.Front()
		if e == nil {
			break
		}
		ic := e.Value.(idleConn)
		p.idle.Remove(e)
		test := p.TestOnBorrow
		p.mu.Unlock()
		if test == nil || test(ic.c, ic.t) == nil {
			return ic.c, nil
		}
		ic.c.Close()
		p.mu.Lock()
		p.active -= 1
	}

	if p.MaxActive > 0 && p.active >= p.MaxActive {
		p.mu.Unlock()
		fmt.Println("REDIGO: ERROR ON GETTING CONNECTION...")
		return nil, ErrPoolExhausted
	}

	// No idle connection, create new.

	dial := p.Dial
	p.active += 1
	p.mu.Unlock()
	c, err := dial()
	if err != nil {
		p.mu.Lock()
		p.active -= 1
		p.mu.Unlock()
		c = nil
	}
	return c, err
}

func (p *Pool) put(c Conn, forceClose bool) error {
	if c.Err() == nil && !forceClose {
		p.mu.Lock()
		if !p.closed {
			p.idle.PushFront(idleConn{t: nowFunc(), c: c})
			if p.idle.Len() > p.MaxIdle {
				c = p.idle.Remove(p.idle.Back()).(idleConn).c
			} else {
				c = nil
			}
		}
		p.mu.Unlock()
	}
	if c != nil {
		p.mu.Lock()
		p.active -= 1
		p.mu.Unlock()
		return c.Close()
	}
	return nil
}

type pooledConnection struct {
	p     *Pool
	c     Conn
	state int
}

var (
	sentinel     []byte
	sentinelOnce sync.Once
)

func initSentinel() {
	p := make([]byte, 64)
	if _, err := rand.Read(p); err == nil {
		sentinel = p
	} else {
		h := sha1.New()
		io.WriteString(h, "Oops, rand failed. Use time instead.")
		io.WriteString(h, strconv.FormatInt(time.Now().UnixNano(), 10))
		sentinel = h.Sum(nil)
	}
}

func (pc *pooledConnection) Close() error {
	c := pc.c
	if _, ok := c.(errorConnection); ok {
		return nil
	}
	pc.c = errorConnection{errConnClosed}

	if pc.state&multiState != 0 {
		c.Send("DISCARD")
		pc.state &^= (multiState | watchState)
	} else if pc.state&watchState != 0 {
		c.Send("UNWATCH")
		pc.state &^= watchState
	}
	if pc.state&subscribeState != 0 {
		c.Send("UNSUBSCRIBE")
		c.Send("PUNSUBSCRIBE")
		// To detect the end of the message stream, ask the server to echo
		// a sentinel value and read until we see that value.
		sentinelOnce.Do(initSentinel)
		c.Send("ECHO", sentinel)
		c.Flush()
		for {
			p, err := c.Receive()
			if err != nil {
				break
			}
			if p, ok := p.([]byte); ok && bytes.Equal(p, sentinel) {
				pc.state &^= subscribeState
				break
			}
		}
	}
	c.Do("")
	pc.p.put(c, pc.state != 0)
	return nil
}

func (pc *pooledConnection) Err() error {
	return pc.c.Err()
}

func (pc *pooledConnection) Do(commandName string, args ...interface{}) (reply interface{}, err error) {
	ci := lookupCommandInfo(commandName)
	pc.state = (pc.state | ci.set) &^ ci.clear
	return pc.c.Do(commandName, args...)
}

func (pc *pooledConnection) Send(commandName string, args ...interface{}) error {
	ci := lookupCommandInfo(commandName)
	pc.state = (pc.state | ci.set) &^ ci.clear
	return pc.c.Send(commandName, args...)
}

func (pc *pooledConnection) Flush() error {
	return pc.c.Flush()
}

func (pc *pooledConnection) Receive() (reply interface{}, err error) {
	return pc.c.Receive()
}

type errorConnection struct{ err error }

func (ec errorConnection) Do(string, ...interface{}) (interface{}, error) { return nil, ec.err }
func (ec errorConnection) Send(string, ...interface{}) error              { return ec.err }
func (ec errorConnection) Err() error                                     { return ec.err }
func (ec errorConnection) Close() error                                   { return ec.err }
func (ec errorConnection) Flush() error                                   { return ec.err }
func (ec errorConnection) Receive() (interface{}, error)                  { return nil, ec.err }
