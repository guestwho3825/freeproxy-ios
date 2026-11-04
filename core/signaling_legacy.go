package core

// Legacy websocket signaling for Realms that report networkProtocol NETHERNET.
// Adapted from GameParrot/netherconnect (MIT License, Copyright (c) 2025 GameParrot).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/df-mc/go-nethernet"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/service"
)

type legacyMsg struct {
	Type uint32      `json:"Type"`
	From string      `json:"From,omitempty"`
	To   json.Number `json:"To,omitempty"`
	Data string      `json:"Message,omitempty"`
}

const (
	legacyPing uint32 = iota
	legacySignal
	legacyCredentials
)

type legacySignaling struct {
	ws        *websocket.Conn
	ctx       context.Context
	networkID string
	log       *slog.Logger

	ready     chan struct{}
	readyOnce sync.Once
	cred      *nethernet.Credentials

	subsMu sync.Mutex
	subs   map[int]nethernet.Notifier
	nextID int
	writeMu sync.Mutex
}

func dialLegacySignaling(ctx context.Context, tok *service.Token, log *slog.Logger) (*legacySignaling, error) {
	disc, err := service.Discover(ctx, service.ApplicationTypeMinecraftPE, protocol.CurrentVersion)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	var env signalingEnv
	if err := disc.Environment(&env); err != nil {
		return nil, err
	}
	u, err := url.Parse(env.ServiceURI)
	if err != nil {
		return nil, err
	}
	id := strconv.FormatUint(rand.Uint64(), 10)
	h := make(http.Header)
	h.Set("Authorization", tok.AuthorizationHeader)
	ws, _, err := websocket.Dial(ctx, u.JoinPath("/ws/v1.0/signaling/", id).String(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	c, cancel := context.WithCancelCause(context.Background())
	s := &legacySignaling{ws: ws, ctx: c, networkID: id, log: log, ready: make(chan struct{}), subs: map[int]nethernet.Notifier{}}
	go s.read(cancel)
	go s.ping()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ready:
		return s, nil
	case <-c.Done():
		return nil, context.Cause(c)
	}
}

func (s *legacySignaling) write(m legacyMsg) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return wsjson.Write(context.Background(), s.ws, m)
}

func (s *legacySignaling) ping() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = s.write(legacyMsg{Type: legacyPing})
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *legacySignaling) read(cancel context.CancelCauseFunc) {
	for {
		var m legacyMsg
		if err := wsjson.Read(context.Background(), s.ws, &m); err != nil {
			cancel(err)
			return
		}
		switch m.Type {
		case legacyCredentials:
			var c nethernet.Credentials
			if json.Unmarshal([]byte(m.Data), &c) == nil {
				s.cred = &c
				s.readyOnce.Do(func() { close(s.ready) })
			}
		case legacySignal:
			sig := &nethernet.Signal{}
			if sig.UnmarshalText([]byte(m.Data)) != nil {
				continue
			}
			sig.NetworkID = m.From
			s.subsMu.Lock()
			list := make([]nethernet.Notifier, 0, len(s.subs))
			for _, n := range s.subs {
				list = append(list, n)
			}
			s.subsMu.Unlock()
			for _, n := range list {
				n.NotifySignal(sig)
			}
		}
	}
}

func (s *legacySignaling) Signal(ctx context.Context, sig *nethernet.Signal) error {
	return s.write(legacyMsg{Type: legacySignal, To: json.Number(sig.NetworkID), Data: sig.String()})
}

func (s *legacySignaling) Notify(n nethernet.Notifier) (stop func()) {
	s.subsMu.Lock()
	id := s.nextID
	s.nextID++
	s.subs[id] = n
	s.subsMu.Unlock()
	return func() {
		s.subsMu.Lock()
		delete(s.subs, id)
		s.subsMu.Unlock()
	}
}

func (s *legacySignaling) Credentials(ctx context.Context) (*nethernet.Credentials, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, context.Cause(s.ctx)
	case <-s.ready:
		return s.cred, nil
	}
}

func (s *legacySignaling) Context() context.Context { return s.ctx }
func (s *legacySignaling) NetworkID() string        { return s.networkID }
func (s *legacySignaling) PongData([]byte)          {}
func (s *legacySignaling) Close() error             { return s.ws.Close(websocket.StatusNormalClosure, "") }
