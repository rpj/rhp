package main

import (
	"encoding/base64"
	"encoding/json"
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

const defaultRedisHost = "localhost"
const defaultRedisPort = 6379
const defaultListenHost = "localhost"
const defaultListenPort = 56545
const defaultUsersFile = "./users.json"

var usersMap map[string]string = nil
var redisDefaultClient *redis.Client = nil
var redisOptions = redis.Options{
	DB: 0,
}

type subscriber struct {
	Channel string
	Addr    string
	User    string
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
	CheckOrigin:     func(_ *http.Request) bool { return true },
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
		if len(authHeader) > 1 {
			log.Panic("too many headers!")
		}

		authComps := strings.Split(authHeader[0], " ")

		if len(authComps) != 2 || authComps[0] != "Basic" {
			return "", fmt.Errorf("bad authComps '%v'", authComps)
		}

		decAuthBytes, err := base64.StdEncoding.DecodeString(authComps[1])

		if err != nil {
			return "", err
		}

		decComps := strings.Split(string(decAuthBytes), ":")

		if len(decComps) != 2 {
			return "", fmt.Errorf("bad decComps")
		}

		if storedPwd, okUser := usersMap[decComps[0]]; okUser {
			if storedPwd == decComps[1] {
				return decComps[0], nil
			} else {
				return "", fmt.Errorf("bad pwd")
			}
		}

		return "", fmt.Errorf("bad user")
	}

	return "", fmt.Errorf("bad auth")
}

func (sh *subscribeHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	dir, file := filepath.Split(req.URL.Path)

	if dir == "/sub/" {
		authedUser, err := checkAuth(req)

		if err != nil {
			fmt.Printf("auth err: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		newSubID := uuid.New()

		sh.Lock.Lock()

		if sh.Pending == nil {
			sh.Pending = map[uuid.UUID]subscriber{}
		}

		sh.Pending[newSubID] = subscriber{file, req.RemoteAddr, authedUser}

		sh.Lock.Unlock()

		fmt.Printf("new sub: %v\n", sh.Pending[newSubID])

		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, newSubID.String())

		return
	}

	w.WriteHeader(http.StatusBadRequest)
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

func parseJSON(path string, intoObj interface{}) error {
	file, err := os.Open(path)

	if err != nil {
		fmt.Fprintf(os.Stderr, "parseJSON unable to open '%s': %v\n", path, err)
		return err
	}

	defer file.Close()

	dec := json.NewDecoder(file)
	err = dec.Decode(intoObj)

	if err != nil {
		fmt.Fprintf(os.Stderr, "parseJSON failed to decode: %v\n", err)
		return err
	}

	return nil
}

func main() {
	listenPort := flag.Uint("p", defaultListenPort, "listen port")
	listenHost := flag.String("H", defaultListenHost, "listen host")
	redisPort := flag.Uint("P", defaultRedisPort, "redis port")
	redisHost := flag.String("r", defaultRedisHost, "redis host")

	flag.Parse()

	if listenHost == nil || listenPort == nil || *listenPort < 1024 || *listenPort > 65535 {
		log.Panic("listen spec")
	}

	if redisPort == nil || redisHost == nil || *redisPort < 0 || *redisPort > 65535 {
		log.Panic("redis spec")
	}

	redisAuth := os.Getenv("REDIS_LOCAL_PWD")

	if len(redisAuth) == 0 {
		log.Panic("Need auth")
	}

	redisOptions.Addr = fmt.Sprintf("%s:%d", *redisHost, *redisPort)
	redisOptions.Password = redisAuth
	rc := redis.NewClient(&redisOptions)

	_, err := rc.Ping().Result()

	if err != nil {
		log.Panic("Ping")
	}

	fmt.Printf("connected to redis://%s\n", redisOptions.Addr)
	redisDefaultClient = rc

	err = parseJSON(defaultUsersFile, &usersMap)

	if err != nil {
		log.Panic(err.Error())
	}

	fmt.Printf("found %d valid users\n", len(usersMap))

	gSubscribeHandler = new(subscribeHandler)
	http.Handle("/sub/", gSubscribeHandler)
	http.HandleFunc("/ws/sub", websocketHandler)

	listenSpec := fmt.Sprintf("%s:%d", *listenHost, *listenPort)
	fmt.Printf("listening on %s\n", listenSpec)

	http.ListenAndServe(listenSpec, nil)
}
