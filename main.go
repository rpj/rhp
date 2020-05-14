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
	"plugin"
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
const defaultPluginsPath = "./plugins"
const pluginEntrySymbolName = "RhpPlugin"

var loadedPlugins = map[string]func(interface{}) (interface{}, error){}
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
			fmt.Println(authComps)
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

	fmt.Println(req.Header)
	fmt.Println(req.Method)
	return "", fmt.Errorf("bad auth")
}

func (sh *subscribeHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if req.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Headers", "authorization")
		w.WriteHeader(http.StatusOK)
		return
	}

	dir, file := filepath.Split(req.URL.Path)

	if dir == "/sub/" {
		authedUser, err := checkAuth(req)

		if err != nil {
			fmt.Printf("auth err: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		newSubID := uuid.New()
		newSub := subscriber{file, req.RemoteAddr, authedUser}

		sh.Lock.Lock()

		if sh.Pending == nil {
			sh.Pending = map[uuid.UUID]subscriber{}
		}

		sh.Pending[newSubID] = newSub

		sh.Lock.Unlock()

		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, newSubID.String())

		fmt.Printf("new sub %v\n", newSub)

		return
	}

	w.WriteHeader(http.StatusBadRequest)
}

func readUntilClose(c *websocket.Conn) {
	for {
		if _, _, err := c.NextReader(); err != nil {
			remoteAddr := c.RemoteAddr()
			fmt.Printf("ws client %v disconnected\n", remoteAddr)

			c.Close()

			wsClientsLock.Lock()
			defer wsClientsLock.Unlock()

			wsClients[remoteAddr].Sub.Close()
			delete(wsClients, remoteAddr)

			break
		}
	}
}

func forwardAllOnto(wsc wsClient) {
	for fwd := range wsc.Sub.Channel() {
		payload := interface{}(fwd.Payload)
		var err error

		for pluginName, pluginFunc := range loadedPlugins {
			payload, err = pluginFunc(payload)
			if err != nil {
				log.Panic(pluginName)
			}
		}

		go func() {
			wsc.Lock.Lock()
			defer wsc.Lock.Unlock()
			wsc.Conn.WriteJSON(payload)
		}()
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
	go forwardAllOnto(wsClients[clientAddr])
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

func loadPlugins(fromPath string) {
	err := filepath.Walk(filepath.ToSlash(fromPath), func(path string, info os.FileInfo, err error) error {
		if filepath.Ext(path) == ".so" {
			pluginLoad, err := plugin.Open(path)

			if err != nil {
				return err
			}

			newPlugin, err := pluginLoad.Lookup(pluginEntrySymbolName)

			if err != nil {
				return err
			}

			fmt.Printf("loaded %s\n", path)
			loadedPlugins[path] = newPlugin.(func(interface{}) (interface{}, error))
		}

		return nil
	})

	if err != nil {
		log.Panic(err.Error())
	}

	fmt.Printf("loaded %d plugins\n", len(loadedPlugins))
}

func main() {
	listenPort := flag.Uint("port", defaultListenPort, "http listen port")
	listenHost := flag.String("listen", defaultListenHost, "http listen host")
	redisPort := flag.Uint("redis-port", defaultRedisPort, "redis server port")
	redisHost := flag.String("redis-host", defaultRedisHost, "redis server host")
	pluginsPath := flag.String("plugins", defaultPluginsPath, "plugins path")
	usersFile := flag.String("users", defaultUsersFile, "users JSON file")

	flag.Parse()

	if listenHost == nil || listenPort == nil || *listenPort < 1024 || *listenPort > 65535 {
		log.Panic("listen spec")
	}

	if redisPort == nil || redisHost == nil || *redisPort < 0 || *redisPort > 65535 {
		log.Panic("redis spec")
	}

	loadPlugins(*pluginsPath)

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

	err = parseJSON(*usersFile, &usersMap)

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
