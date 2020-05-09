package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-redis/redis/v7"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const defaultListenPort = 56545

var redisDefaultClient *redis.Client = nil
var redisOptions = redis.Options{
	Addr: "localhost:6379",
	DB:   0,
}

type subscriber struct {
	Channel string
	Addr    string
}

type subscribeHandler struct {
	Lock    sync.Mutex
	Pending map[uuid.UUID]subscriber
}

var gSubscribeHandler *subscribeHandler = nil

func allowLocalhostOnly(r *http.Request) bool {
	addrSplit := strings.Split(r.RemoteAddr, ":")
	return addrSplit[0] == "127.0.0.1" || addrSplit[0] == "localhost"
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     allowLocalhostOnly,
}

type wsClient struct {
	Conn *websocket.Conn
	Lock *sync.Mutex
	Sub  *redis.PubSub
}

var wsClients = map[net.Addr]wsClient{}
var wsClientsLock = sync.Mutex{}

func checkAuth(req *http.Request) (string, error) {
	if authHeader, ok := req.Header["Authorization"]; ok {
		return authHeader[0], nil
	}

	return "", fmt.Errorf("bad auth")
}

func (sh *subscribeHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	dir, file := filepath.Split(req.URL.Path)

	if dir == "/sub/" {
		newSubID := uuid.New()

		sh.Lock.Lock()

		if sh.Pending == nil {
			sh.Pending = map[uuid.UUID]subscriber{}
		}

		sh.Pending[newSubID] = subscriber{file, req.RemoteAddr}

		sh.Lock.Unlock()

		fmt.Printf("Sub req! %s\n", file)
		fmt.Println(sh.Pending)

		fmt.Fprintln(w, newSubID.String())
	}
}

func readUntilClose(c *websocket.Conn) {
	for {
		if _, _, err := c.NextReader(); err != nil {
			fmt.Printf("ws client %v disconnected\n", c.RemoteAddr())

			c.Close()

			wsClientsLock.Lock()
			defer wsClientsLock.Unlock()

			delete(wsClients, c.RemoteAddr())

			break
		}
	}
}

func forwardAllOnto(subChan <-chan *redis.Message, c *websocket.Conn) {
	for fwd := range subChan {
		c.WriteJSON(fwd.Payload)
	}
}

func registerNewClient(wsConn *websocket.Conn, channel string) {
	clientAddr := wsConn.RemoteAddr()

	wsClientsLock.Lock()
	defer wsClientsLock.Unlock()

	if curClient, ok := wsClients[clientAddr]; ok {
		fmt.Printf("already have conn for %v! closing it\n", clientAddr)
		curClient.Lock.Lock()
		curClient.Conn.Close()
		curClient.Lock.Unlock()
	}

	wsClients[clientAddr] = wsClient{wsConn, new(sync.Mutex), redisDefaultClient.Subscribe(channel)}
	fmt.Printf("ws client %v connected\n", clientAddr)

	go readUntilClose(wsConn)
	go forwardAllOnto(wsClients[clientAddr].Sub.Channel(), wsConn)
}

func websocketHandler(w http.ResponseWriter, req *http.Request) {
	wsConn, err := wsUpgrader.Upgrade(w, req, nil)

	if err != nil {
		fmt.Printf("websocketHandler upgrade failed: %v\n", err)
		return
	}

	okReqUUID, err := uuid.Parse(req.URL.RawQuery)

	if err != nil {
		fmt.Printf("bad ws query '%s'\n", req.URL.RawQuery)
		return
	}

	gSubscribeHandler.Lock.Lock()
	defer gSubscribeHandler.Lock.Unlock()

	if pendingConn, ok := gSubscribeHandler.Pending[okReqUUID]; ok {
		if strings.Split(wsConn.RemoteAddr().String(), ":")[0] == strings.Split(pendingConn.Addr, ":")[0] {
			go registerNewClient(wsConn, pendingConn.Channel)
			delete(gSubscribeHandler.Pending, okReqUUID)
		} else {
			fmt.Printf("bad addr match %s vs %s\n", wsConn.RemoteAddr(), pendingConn.Addr)
		}
	} else {
		fmt.Printf("bad pending connection '%v'\n", okReqUUID)
	}
}

func main() {
	listenPort := flag.Uint("p", defaultListenPort, "listen port")

	flag.Parse()

	if listenPort == nil || *listenPort < 1024 || *listenPort > 65535 {
		log.Panic("listen port")
	}

	redisAuth := os.Getenv("REDIS_LOCAL_PWD")

	if len(redisAuth) == 0 {
		log.Panic("Need auth")
	}

	redisOptions.Password = redisAuth
	rc := redis.NewClient(&redisOptions)

	_, err := rc.Ping().Result()

	if err != nil {
		log.Panic("Ping")
	}

	fmt.Println(rc)
	redisDefaultClient = rc

	gSubscribeHandler = new(subscribeHandler)
	http.Handle("/sub/", gSubscribeHandler)
	http.HandleFunc("/ws/sub", websocketHandler)

	listenSpec := fmt.Sprintf("localhost:%d", *listenPort)
	fmt.Printf("listening on %s\n", listenSpec)

	http.ListenAndServe(listenSpec, nil)
}
