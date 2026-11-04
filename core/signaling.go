package core

// JSON-RPC signaling used by newer Realms (networkProtocol NETHERNET_JSONRPC).
// Adapted from GameParrot/netherconnect (MIT License, Copyright (c) 2025 GameParrot),
// changed to work as a dialing client: signals are broadcast to every subscriber
// and stopping a subscription does not close the websocket.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/creachadair/jrpc2"
	"github.com/df-mc/go-nethernet"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/service"
)

type signalingEnv struct {
	ServiceURI string `json:"serviceUri"`
	StunURI    string `json:"stunUri"`
	TurnURI    string `json:"turnUri"`
}

func (e *signalingEnv) ServiceName() string { return "signaling" }

type jsonSignaling struct {
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelCauseFunc

	ws        *websocket.Conn
	client    *jrpc2.Client
	networkID string

	subsMu sync.Mutex
	subs   map[int]nethernet.Notifier
	nextID int

	credMu   sync.Mutex
	cred     *nethernet.Credentials
	credTime time.Time
	once     sync.Once
}

func dialSignaling(ctx context.Context, mct *service.Token, log *slog.Logger) (*jsonSignaling, error) {
	disc, err := service.Discover(ctx, service.ApplicationTypeMinecraftPE, protocol.CurrentVersion)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	var env signalingEnv
	if err := disc.Environment(&env); err != nil {
		return nil, fmt.Errorf("signaling environment: %w", err)
	}
	u, err := url.Parse(env.ServiceURI)
	if err != nil {
		return nil, err
	}
	h := make(http.Header)
	h.Set("Authorization", mct.AuthorizationHeader)
	h.Set("User-Agent", "libHttpClient/1.0.0.0")
	ws, _, err := websocket.Dial(context.Background(), u.JoinPath("/ws/v1.0/messaging/connect").String(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, fmt.Errorf("connect signaling: %w", err)
	}
	ws.SetReadLimit(4 << 20)
	id, err := messagingID(mct)
	if err != nil {
		_ = ws.Close(websocket.StatusNormalClosure, "")
		return nil, err
	}
	c, cancel := context.WithCancelCause(context.Background())
	s := &jsonSignaling{log: log, ctx: c, cancel: cancel, ws: ws, networkID: id.String(), subs: map[int]nethernet.Notifier{}}
	s.client = jrpc2.NewClient(&wsChannel{ws}, &jrpc2.ClientOptions{
		OnCallback: s.callback,
		OnStop: func(_ *jrpc2.Client, err error) {
			if err == nil {
				err = errors.New("signaling closed")
			}
			s.cancel(err)
		},
	})
	go s.ping()
	return s, nil
}

func messagingID(mct *service.Token) (uuid.UUID, error) {
	tok, err := jwt.ParseSigned(strings.TrimPrefix(mct.AuthorizationHeader, "MCToken "), []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("parse service token: %w", err)
	}
	var claims struct {
		PMID uuid.UUID `json:"pmid"`
	}
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return uuid.UUID{}, err
	}
	return claims.PMID, nil
}

func (s *jsonSignaling) ping() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_, _ = s.client.Call(context.Background(), "System_Ping_v1_0", map[string]any{})
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *jsonSignaling) broadcast(sig *nethernet.Signal) {
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

func (s *jsonSignaling) callback(ctx context.Context, req *jrpc2.Request) (any, error) {
	if req.Method() != "Signaling_ReceiveMessage_v1_0" {
		return nil, nil
	}
	var batch []struct {
		From  uuid.UUID
		Inner string `json:"Message"`
		ID    string `json:"Id"`
	}
	if err := req.UnmarshalParams(&batch); err != nil {
		s.log.Debug("signaling: bad batch", "err", err)
		return nil, nil
	}
	for _, msg := range batch {
		var inner struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(msg.Inner), &inner); err != nil || inner.Method != "Signaling_WebRtc_v1_0" {
			continue
		}
		var p struct {
			NetherNetID string `json:"netherNetId"`
			Data        string `json:"message"`
		}
		if err := json.Unmarshal(inner.Params, &p); err != nil || p.Data == "" {
			continue
		}
		sig := &nethernet.Signal{NetworkID: msg.From.String()}
		if err := sig.UnmarshalText([]byte(p.Data)); err != nil {
			continue
		}
		s.broadcast(sig)

		ack, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "Signaling_DeliveryNotification_V1_0",
			"params":  map[string]any{"netherNetId": s.networkID},
		})
		go func(to uuid.UUID, id string) {
			_, _ = s.client.Call(context.Background(), "Signaling_SendClientMessage_v1_0", map[string]any{
				"toPlayerId": to, "messageId": id, "message": string(ack),
			})
		}(msg.From, msg.ID)
	}
	return nil, nil
}

func (s *jsonSignaling) Signal(ctx context.Context, sig *nethernet.Signal) error {
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "Signaling_WebRtc_v1_0",
		"params":  map[string]any{"netherNetId": s.networkID, "message": sig.String()},
	})
	if err != nil {
		return err
	}
	to, err := uuid.Parse(sig.NetworkID)
	if err != nil {
		return fmt.Errorf("realm network id is not a uuid: %w", err)
	}
	resp, err := s.client.Call(ctx, "Signaling_SendClientMessage_v1_0", map[string]any{
		"toPlayerId": to, "messageId": uuid.New(), "message": string(b),
	})
	if err != nil {
		return err
	}
	if resp.Error() != nil {
		return resp.Error()
	}
	return nil
}

func (s *jsonSignaling) Notify(n nethernet.Notifier) (stop func()) {
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

func (s *jsonSignaling) Context() context.Context { return s.ctx }
func (s *jsonSignaling) NetworkID() string        { return s.networkID }
func (s *jsonSignaling) PongData([]byte)          {}

func (s *jsonSignaling) Credentials(ctx context.Context) (*nethernet.Credentials, error) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if s.cred != nil && time.Now().Before(s.credTime.Add(time.Duration(s.cred.ExpirationInSeconds)*time.Second)) {
		return s.cred, nil
	}
	var c *nethernet.Credentials
	if err := s.client.CallResult(ctx, "Signaling_TurnAuth_v1_0", map[string]any{}, &c); err != nil {
		return nil, fmt.Errorf("turn auth: %w", err)
	}
	if c == nil || c.ExpirationInSeconds == 0 {
		return nil, errors.New("turn auth: invalid result")
	}
	s.cred, s.credTime = c, time.Now()
	return c, nil
}

func (s *jsonSignaling) Close() error {
	var err error
	s.once.Do(func() {
		err = errors.Join(s.client.Close(), s.ws.Close(websocket.StatusGoingAway, ""))
	})
	return err
}

type wsChannel struct{ *websocket.Conn }

func (c *wsChannel) Send(b []byte) error {
	return c.Write(context.Background(), websocket.MessageText, b)
}
func (c *wsChannel) Recv() ([]byte, error) {
	_, b, err := c.Read(context.Background())
	return b, err
}
func (c *wsChannel) Close() error { return c.Conn.Close(websocket.StatusGoingAway, "") }
