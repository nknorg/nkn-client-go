package nkn_sdk_go

import (
	"encoding/hex"
	"errors"
	"log"
	"net"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/imdario/mergo"
	"github.com/nknorg/nkn/util/address"
	"github.com/nknorg/nkn/vault"

	"github.com/patrickmn/go-cache"

	"github.com/nknorg/ncp"
	"github.com/nknorg/nkn-sdk-go/payloads"
)

const (
	identifierRe  = "^__\\d+__$"
	SessionIDSize = 8 // in bytes
)

type MultiClient struct {
	config        *ClientConfig
	offset        int
	Clients       map[int]*Client
	DefaultClient *Client
	addr          Addr
	Address       string
	OnConnect     chan struct{}
	OnMessage     chan *Message
	acceptSession chan *ncp.Session
	onClose       chan struct{}

	sync.RWMutex
	sessions map[string]*ncp.Session
	isClosed bool
}

func NewMultiClient(account *vault.Account, baseIdentifier string, numSubClients int, originalClient bool, configs ...ClientConfig) (*MultiClient, error) {
	config, err := MergedClientConfig(configs)
	if err != nil {
		return nil, err
	}

	numClients := numSubClients
	offset := 0
	if originalClient {
		numClients++
		offset = 1
	}

	clients := make(map[int]*Client, numClients)

	var wg sync.WaitGroup
	var lock sync.Mutex
	success := false
	for i := -offset; i < numSubClients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client, err := NewClient(account, addIdentifier(baseIdentifier, i), configs...)
			if err != nil {
				log.Println(err)
				return
			}
			lock.Lock()
			clients[i] = client
			success = true
			lock.Unlock()
		}(i)
	}
	wg.Wait()
	if !success {
		return nil, errors.New("failed to create any client")
	}

	var defaultClient *Client
	if originalClient {
		defaultClient = clients[-1]
	} else {
		defaultClient = clients[0]
	}

	addr := address.MakeAddressString(account.PublicKey.EncodePoint(), baseIdentifier)

	onConnect := make(chan struct{}, 1)
	go func() {
		cases := make([]reflect.SelectCase, numClients)
		for i := 0; i < numClients; i++ {
			if clients[i-offset] != nil {
				cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(clients[i-offset].OnConnect)}
			} else {
				cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv}
			}
		}
		if _, _, ok := reflect.Select(cases); ok {
			onConnect <- struct{}{}
		}
	}()

	onMessage := make(chan *Message, config.MsgChanLen)

	m := &MultiClient{
		config:        config,
		offset:        offset,
		Clients:       clients,
		DefaultClient: defaultClient,
		addr:          Addr{addr: addr},
		Address:       addr,
		OnConnect:     onConnect,
		OnMessage:     onMessage,
		acceptSession: make(chan *ncp.Session, 128),
		sessions:      make(map[string]*ncp.Session, 0),
		onClose:       make(chan struct{}, 0),
	}

	c := cache.New(config.MsgCacheExpiration, config.MsgCacheExpiration)
	go func() {
		cases := make([]reflect.SelectCase, numClients)
		for i := 0; i < numClients; i++ {
			if clients[i-offset] != nil {
				cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(clients[i-offset].OnMessage)}
			} else {
				cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv}
			}
		}
		for {
			select {
			case _, ok := <-m.onClose:
				if !ok {
					return
				}
			default:
			}
			if i, value, ok := reflect.Select(cases); ok {
				msg := value.Interface().(*Message)
				if msg.IsSession {
					err := m.handleSessionMsg(addIdentifier("", i-offset), msg.Src, msg.Pid, msg.Data)
					if err != nil {
						if err != ncp.SessionClosed {
							log.Println(err)
						}
						continue
					}
				} else {
					cacheKey := string(msg.Pid)
					if _, ok := c.Get(cacheKey); ok {
						continue
					}
					c.Set(cacheKey, struct{}{}, cache.DefaultExpiration)

					msg.Src, _ = removeIdentifier(msg.Src)
					msg.Reply = func(response []byte) {
						pid := msg.Pid
						var payload *payloads.Payload
						var err error
						if response == nil {
							payload, err = newAckPayload(pid)
						} else {
							payload, err = newBinaryPayload(response, pid)
						}
						if err != nil {
							log.Println("Problem creating response to PID " + hex.EncodeToString(pid))
						}
						if err := m.send([]string{msg.Src}, payload, msg.Encrypted); err != nil {
							log.Println("Problem sending response to PID " + hex.EncodeToString(pid))
						}
					}
					onMessage <- msg
				}
			}
		}
	}()

	return m, nil
}

func (m *MultiClient) SendWithClient(clientID int, dests []string, data []byte, encrypted bool, MaxHoldingSeconds ...uint32) (*Message, error) {
	payload, err := newBinaryPayload(data, nil)
	if err != nil {
		return nil, err
	}
	pidString := string(payload.Pid)
	responseChannel := make(chan *Message, 1)
	c := m.Clients[clientID]
	c.responseChannels[pidString] = responseChannel
	if err := m.sendWithClient(clientID, dests, payload, encrypted, MaxHoldingSeconds...); err != nil {
		return nil, err
	}
	msg := <-responseChannel
	msg.Src, _ = removeIdentifier(msg.Src)
	return msg, nil
}

func (m *MultiClient) sendWithClient(clientID int, dests []string, payload *payloads.Payload, encrypted bool, MaxHoldingSeconds ...uint32) error {
	c := m.Clients[clientID]
	return c.send(processDest(dests, clientID), payload, encrypted, MaxHoldingSeconds...)
}

func (m *MultiClient) Send(dests []string, data []byte, encrypted bool, MaxHoldingSeconds ...uint32) (*Message, error) {
	payload, err := newBinaryPayload(data, nil)
	if err != nil {
		return nil, err
	}
	responseChannels := make([]chan *Message, len(m.Clients))
	pidString := string(payload.Pid)
	offset := m.offset
	for clientID, c := range m.Clients {
		responseChannel := make(chan *Message, 1)
		responseChannels[clientID+offset] = responseChannel
		c.responseChannels[pidString] = responseChannel
		if err := m.sendWithClient(clientID, dests, payload, encrypted, MaxHoldingSeconds...); err != nil {
			return nil, err
		}
	}
	cases := make([]reflect.SelectCase, len(responseChannels))
	for i, responseChannel := range responseChannels {
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(responseChannel)}
	}
	if _, value, ok := reflect.Select(cases); ok {
		msg := value.Interface().(*Message)
		msg.Src, _ = removeIdentifier(msg.Src)
		return msg, nil
	}
	return nil, errors.New("error reading response channel")
}

func (m *MultiClient) send(dests []string, payload *payloads.Payload, encrypted bool, MaxHoldingSeconds ...uint32) error {
	for clientID := range m.Clients {
		if err := m.sendWithClient(clientID, dests, payload, encrypted, MaxHoldingSeconds...); err != nil {
			return err
		}
	}
	return nil
}

func (m *MultiClient) newSession(remoteAddr string, sessionID []byte, config *SessionConfig) (*ncp.Session, error) {
	clientIDs := make([]string, 0, len(m.Clients))
	clients := make(map[string]*Client, len(m.Clients))
	for id, client := range m.Clients {
		clientID := addIdentifier("", id)
		clientIDs = append(clientIDs, clientID)
		clients[clientID] = client
	}
	sort.Strings(clientIDs)
	return ncp.NewSession(m.addr, Addr{addr: remoteAddr}, clientIDs, nil, (func(localClientID, remoteClientID string, buf []byte, writeTimeout time.Duration) error {
		payload := &payloads.Payload{
			Type:      payloads.BINARY,
			Pid:       sessionID,
			Data:      buf,
			IsSession: true,
		}
		c := clients[localClientID]
		if writeTimeout > 0 {
			err := c.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err != nil {
				return err
			}
		}
		err := c.send([]string{addIdentifierPrefix(remoteAddr, remoteClientID)}, payload, true, 0)
		if err != nil {
			return err
		}
		if writeTimeout > 0 {
			err = c.SetWriteDeadline(zeroTime)
			if err != nil {
				return err
			}
		}
		return nil
	}), (*ncp.Config)(config))
}

func (m *MultiClient) handleSessionMsg(localClientID, src string, sessionID, data []byte) error {
	remoteAddr, remoteClientID := removeIdentifier(src)
	sessionKey := sessionKey(remoteAddr, sessionID)

	m.Lock()
	session, ok := m.sessions[sessionKey]
	if !ok {
		session, err := m.newSession(remoteAddr, sessionID, &m.config.SessionConfig)
		if err != nil {
			m.Unlock()
			return err
		}

		m.sessions[sessionKey] = session
		m.Unlock()

		err = session.ReceiveWith(localClientID, remoteClientID, data)
		if err != nil {
			return err
		}

		select {
		case m.acceptSession <- session:
		default:
			log.Println("Accept session channel full, discard request...")
		}
	} else {
		m.Unlock()
		err := session.ReceiveWith(localClientID, remoteClientID, data)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *MultiClient) Addr() net.Addr {
	return m.addr
}

func (m *MultiClient) Dial(remoteAddr string) (*ncp.Session, error) {
	return m.DialWithConfig(remoteAddr, nil)
}

func (m *MultiClient) DialWithConfig(remoteAddr string, config *SessionConfig) (*ncp.Session, error) {
	merged := m.config.SessionConfig
	if config != nil {
		err := mergo.Merge(&merged, config, mergo.WithOverride)
		if err != nil {
			return nil, err
		}
	}
	config = &merged

	sessionID, err := RandomBytes(SessionIDSize)
	if err != nil {
		return nil, err
	}
	sessionKey := sessionKey(remoteAddr, sessionID)
	session, err := m.newSession(remoteAddr, sessionID, config)
	if err != nil {
		return nil, err
	}

	m.Lock()
	m.sessions[sessionKey] = session
	m.Unlock()

	err = session.Dial()
	if err != nil {
		return nil, err
	}

	return session, nil
}

func (m *MultiClient) AcceptSession() (*ncp.Session, error) {
	for {
		select {
		case session := <-m.acceptSession:
			err := session.Accept()
			if err != nil {
				log.Println(err)
				continue
			}
			return session, nil
		case _, ok := <-m.onClose:
			if !ok {
				return nil, ncp.Closed
			}
		}
	}
}

func (m *MultiClient) Accept() (net.Conn, error) {
	return m.AcceptSession()
}

func (m *MultiClient) Close() error {
	m.Lock()
	defer m.Unlock()

	if m.isClosed {
		return nil
	}

	for _, session := range m.sessions {
		err := session.Close()
		if err != nil {
			log.Println(err)
			continue
		}
	}

	for _, client := range m.Clients {
		client.Close()
	}

	m.isClosed = true

	close(m.onClose)

	return nil
}

func (m *MultiClient) IsClosed() bool {
	m.RLock()
	defer m.RUnlock()
	return m.isClosed
}