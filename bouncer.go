package main

import (
	"context"
	"log"
	"net/http"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
	"sync"
	"time"
)

type SessionSubIDs map[string]*[]interface{}
type SessionEventIDs map[string]map[string]struct{}
type SessionPendingEOSE map[string]int
type SessionRelays map[*websocket.Conn]context.Context
type SessionUpstreamMessage chan *[]interface{}
type SessionDoneChannel chan struct{}
type SessionRelayCancelContext map[context.Context]context.CancelFunc

type Session struct {
	ClientIP string

	Sub_IDs     SessionSubIDs
	Event_IDs   SessionEventIDs
	PendingEOSE SessionPendingEOSE
	Relays      SessionRelays
	CancelZone  SessionRelayCancelContext

	UpstreamMessage SessionUpstreamMessage
	Done            SessionDoneChannel
	ready           bool
	destroyed       bool

	eventMu     sync.Mutex
	eoseMu      sync.Mutex
	relaysMu    sync.Mutex
	connWriteMu sync.Mutex
	subMu       sync.Mutex
	cancelMu    sync.Mutex
}

func (s *Session) NewConn(url string) {
loop:
	for {
		if s.destroyed {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

		s.cancelMu.Lock()
		s.CancelZone[ctx] = cancel
		s.cancelMu.Unlock()

		connHeaders := make(http.Header)
		connHeaders.Add("User-Agent", "Blyat; Nostr relay bouncer; https://github.com/Yonle/blyat")

		conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
			HTTPHeader:      connHeaders,
			CompressionMode: websocket.CompressionContextTakeover,
		})

		s.cancelMu.Lock()
		delete(s.CancelZone, ctx)
		s.cancelMu.Unlock()

		if s.destroyed && err == nil {
			cancel()
			conn.CloseNow()
			return
		}

		if err != nil {
			cancel()
			log.Printf("%s Произошла ошибка при подключении к %s. Повторная попытка через 5 секунд....\n", s.ClientIP, url)
			time.Sleep(5 * time.Second)
			continue loop
		}

		if resp.StatusCode >= 500 {
			cancel()
			conn.CloseNow()
			log.Printf("%s Произошла ошибка при подключении к %s. Повторная попытка через 5 секунд....\n", s.ClientIP, url)
			time.Sleep(5 * time.Second)
			continue loop
		} else if resp.StatusCode > 101 {
			cancel()
			conn.CloseNow()
			log.Printf("%s Получил неожиданный код статуса от %s (%d). Больше не подключаюсь.\n", s.ClientIP, url, resp.StatusCode)
			return
		}

		s.relaysMu.Lock()
		s.Relays[conn] = ctx
		s.relaysMu.Unlock()

		log.Printf("%s %s связанный\n", s.ClientIP, url)

		s.OpenSubscriptions(ctx, conn)

		for {
			var data []interface{}
			if err := wsjson.Read(ctx, conn, &data); err != nil {
				break
			}

			if data == nil {
				continue
			}

			switch data[0].(string) {
			case "EVENT":
				s.HandleUpstreamEVENT(data)
			case "EOSE":
				s.HandleUpstreamEOSE(data)
			}
		}

		conn.CloseNow()
		cancel()

		if s.destroyed {
			log.Printf("%s %s: Отключение\n", s.ClientIP, url)
			return
		}

		log.Printf("%s Произошла ошибка при подключении к %s. Повторная попытка через 5 секунд....\n", s.ClientIP, url)

		s.relaysMu.Lock()
		delete(s.Relays, conn)
		s.relaysMu.Unlock()

		time.Sleep(5 * time.Second)

		if s.destroyed {
			return
		}
	}
}

func (s *Session) StartConnect() {
	log.Println(s.ClientIP, "Начало сеанса....")
	for _, url := range config.Relays {
		if s.destroyed {
			break
		}
		go s.NewConn(url)
	}
}

func (s *Session) Broadcast(data *[]interface{}) {
	s.relaysMu.Lock()
	defer s.relaysMu.Unlock()

	for relay, ctx := range s.Relays {
		s.connWriteMu.Lock()
		wsjson.Write(ctx, relay, data)
		s.connWriteMu.Unlock()
	}
}

func (s *Session) HasEvent(subid string, event_id string) bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	events := s.Event_IDs[subid]
	if events == nil {
		return true
	}

	_, ok := events[event_id]

	if !ok {
		events[event_id] = struct{}{}
	}

	if len(events) > 500 {
		s.eoseMu.Lock()
		if _, ok := s.PendingEOSE[subid]; ok {
			delete(s.PendingEOSE, subid)
			s.WriteJSON(&[]interface{}{"EOSE", subid})
		}
		s.eoseMu.Unlock()
	}

	return ok
}

func (s *Session) HandleUpstreamEVENT(data []interface{}) {
	if len(data) < 3 {
		return
	}

	s.subMu.Lock()
	if _, ok := s.Sub_IDs[data[1].(string)]; !ok {
		s.subMu.Unlock()
		return
	}
	s.subMu.Unlock()

	if event := data[2].(map[string]interface{}); s.HasEvent(data[1].(string), event["id"].(string)) {
		return
	}

	s.WriteJSON(&data)
}

func (s *Session) HandleUpstreamEOSE(data []interface{}) {
	if len(data) < 2 {
		return
	}

	s.eoseMu.Lock()
	defer s.eoseMu.Unlock()

	if _, ok := s.PendingEOSE[data[1].(string)]; !ok {
		return
	}

	s.PendingEOSE[data[1].(string)]++
	if s.PendingEOSE[data[1].(string)] >= len(config.Relays) {
		delete(s.PendingEOSE, data[1].(string))
		s.WriteJSON(&data)
	}
}

/*
func (s *Session) CountEvents(subid string) int {
  return len(s.Event_IDs[subid])
}
*/

func (s *Session) WriteJSON(data *[]interface{}) {
	s.UpstreamMessage <- data
}

func (s *Session) OpenSubscriptions(ctx context.Context, conn *websocket.Conn) {
	s.subMu.Lock()
	defer s.subMu.Unlock()

	for id, filters := range s.Sub_IDs {
		ReqData := []interface{}{"REQ", id}
		ReqData = append(ReqData, *filters...)

		s.connWriteMu.Lock()
		wsjson.Write(ctx, conn, ReqData)
		s.connWriteMu.Unlock()
	}
}

func (s *Session) Destroy() {
	s.destroyed = true

	s.cancelMu.Lock()
	for _, cancel := range s.CancelZone {
		cancel()
	}
	s.cancelMu.Unlock()

	s.relaysMu.Lock()
	for relay := range s.Relays {
		relay.CloseNow()
	}
	s.relaysMu.Unlock()

	s.Done <- struct{}{}
}

func (s *Session) REQ(data *[]interface{}) {
	if !s.ready {
		s.StartConnect()
		s.ready = true
	}

	subid := (*data)[1].(string)
	filters := (*data)[2:]

	s.CLOSE(data, false)

	s.eventMu.Lock()
	s.Event_IDs[subid] = make(map[string]struct{})
	s.eventMu.Unlock()

	s.eoseMu.Lock()
	s.PendingEOSE[subid] = 0
	s.eoseMu.Unlock()

	s.subMu.Lock()
	s.Sub_IDs[subid] = &filters
	s.subMu.Unlock()

	s.Broadcast(data)
}

func (s *Session) CLOSE(data *[]interface{}, sendClosed bool) {
	subid := (*data)[1].(string)

	s.eventMu.Lock()
	delete(s.Event_IDs, subid)
	s.eventMu.Unlock()

	s.subMu.Lock()
	delete(s.Sub_IDs, subid)
	s.subMu.Unlock()

	s.eoseMu.Lock()
	delete(s.PendingEOSE, subid)
	s.eoseMu.Unlock()

	if sendClosed {
		s.WriteJSON(&[]interface{}{"CLOSED", subid, ""})
	}

	s.Broadcast(data)
}

func (s *Session) EVENT(data *[]interface{}) {
	if !s.ready {
		s.StartConnect()
		s.ready = true
	}

	event := (*data)[1].(map[string]interface{})
	id, ok := event["id"]
	if !ok {
		s.WriteJSON(&[]interface{}{"NOTICE", "Неверный объект."})
		return
	}

	s.WriteJSON(&[]interface{}{"OK", id, true, ""})
	s.Broadcast(data)
}
