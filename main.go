package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"plugin"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v7"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const defaultRedisHost = "localhost"
const defaultRedisPort = 6379
const defaultListenHost = "localhost"
const defaultListenPort = 56545
const defaultUsersFile = "./users.json"
const defaultPublishChannelPrefix = "rhp"
const defaultPublishChannelsFile = "./publish.json"
const defaultPluginsPath = "./build/plugins"

var loadedPlugins rhpPluginsT
var g_usersFile string
var usersMap map[string]string = nil
var usersMapLock sync.Mutex = sync.Mutex{}
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

type publishSpec struct {
	ChannelId string
	Message   string
	User      string
}

var publishMap map[string]string = nil
var publishChan = make(chan publishSpec)
var publishLock sync.Mutex = sync.Mutex{}

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
			log.Println(authComps)
			return "", err
		}

		decComps := strings.Split(string(decAuthBytes), ":")

		if len(decComps) != 2 {
			return "", fmt.Errorf("bad decComps")
		}

		usersMapLock.Lock()
		defer usersMapLock.Unlock()
		if storedPwd, okUser := usersMap[decComps[0]]; okUser {
			if storedPwd == decComps[1] {
				return decComps[0], nil
			} else {
				return "", fmt.Errorf("bad pwd")
			}
		}

		return "", fmt.Errorf("bad user")
	}

	log.Println(req.Header)
	log.Println(req.Method)
	return "", fmt.Errorf("bad auth")
}

func (sh *subscribeHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if req.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Headers", "authorization")
		w.WriteHeader(http.StatusOK)
		return
	}

	authedUser, err := checkAuth(req)

	if err != nil {
		log.Printf("auth err: %v\n", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dir, file := filepath.Split(req.URL.Path)

	var respStr string = ""

	if dir == "/sub/" {
		newSubID := uuid.New()
		newSub := subscriber{file, req.RemoteAddr, authedUser}

		sh.Lock.Lock()
		if sh.Pending == nil {
			sh.Pending = map[uuid.UUID]subscriber{}
		}

		sh.Pending[newSubID] = newSub
		sh.Lock.Unlock()

		log.Printf("new sub %v\n", newSub)

		respStr = newSubID.String()
	} else if dir == "/pub/" && file != "" && req.Method == "POST" {
		bodyBytes, err := io.ReadAll(req.Body)

		if err != nil {
			log.Printf("Failed to read pub req body: %v", req)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		publishChan <- publishSpec{file, string(bodyBytes), authedUser}
		w.WriteHeader(http.StatusAccepted)
		return
	} else if dir == "/list/" && file != "" {
		query := req.URL.Query()
		listLookup := func(start int64, end int64) ([]string, error) {
			return redisDefaultClient.LRange(file, start, end).Result()
		}

		// allow plugins the opprotunity to handle the request before using the default handler
		// only one can handle any given request, so the first to do so affirmatively ends the request
		loadedPlugins.Lock.Lock()
		for name, plugin := range loadedPlugins.List {
			strResp, err := plugin.HandleListReq(dir, file, query, listLookup)

			if err == nil && len(strResp) > 0 {
				respStr = strResp
				log.Printf("using response for '%v%v' produced by plugin '%v'\n", dir, file, name)
				break
			}
		}
		loadedPlugins.Lock.Unlock()

		// default handler
		if respStr == "" {
			start := int64(0)
			end := int64(10)
			var err error = nil
			log.Printf("LIST -- %v -- %v\n", file, query)

			if startSpec, ok := query["start"]; ok {
				start, err = strconv.ParseInt(startSpec[0], 10, 64)
			}

			if err == nil && start >= 0 {
				if endSpec, ok := query["end"]; ok {
					end, err = strconv.ParseInt(endSpec[0], 10, 64)
				}

				if err == nil && end > start {
					lRes := redisDefaultClient.LRange(file, start, end)

					listRes, err := lRes.Result()

					if err == nil {
						listStr, err := json.Marshal(listRes)
						if err == nil {
							respStr = string(listStr)
						}
					}
				}
			}
		}
	}

	if respStr != "" {
		_, err := w.Write([]byte(respStr))

		if err != nil {
			log.Printf("failed to write response: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	} else {
		log.Printf("BAD REQ: %v\n", req)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func readUntilClose(c *websocket.Conn) {
	log.Printf("readUntilClose for %v STARTING\n", c.RemoteAddr())
	for {
		if _, _, err := c.NextReader(); err != nil {
			remoteAddr := c.RemoteAddr()
			log.Printf("ws client %v disconnected\n", remoteAddr)

			c.Close()

			wsClientsLock.Lock()
			defer wsClientsLock.Unlock()

			wsClients[remoteAddr].Sub.Close()
			delete(wsClients, remoteAddr)

			break
		}
	}
	log.Printf("readUntilClose for %v EXITING\n", c.RemoteAddr())
}

func forwardAllOnto(wsc wsClient) {
	log.Printf("forAllOnto for %v STARTING\n", wsc.Conn.RemoteAddr())
	for fwd := range wsc.Sub.Channel() {
		payload := interface{}(fwd.Payload)
		var err error

		loadedPlugins.Lock.Lock()
		for pluginName, plugin := range loadedPlugins.List {
			payload, err = plugin.HandleMsg(payload)
			if err != nil {
				log.Panic(pluginName)
			}
		}
		loadedPlugins.Lock.Unlock()

		go func() {
			wsc.Lock.Lock()
			defer wsc.Lock.Unlock()
			wsc.Conn.WriteJSON(payload)
		}()
	}
	log.Printf("forAllOnto for %v EXITING\n", wsc.Conn.RemoteAddr())
}

func registerNewClient(wsConn *websocket.Conn, channel string) {
	clientAddr := wsConn.RemoteAddr()

	wsClientsLock.Lock()
	defer wsClientsLock.Unlock()

	if curClient, ok := wsClients[clientAddr]; ok {
		log.Printf("already have conn for %v! closing it\n", clientAddr)
		curClient.Lock.Lock()
		curClient.Conn.Close()
		curClient.Lock.Unlock()
	}

	wsClients[clientAddr] = wsClient{wsConn, new(sync.Mutex), redisDefaultClient.Subscribe(channel)}
	log.Printf("ws client %v connected\n", clientAddr)

	go readUntilClose(wsConn)
	go forwardAllOnto(wsClients[clientAddr])
}

func refreshHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != "OPTIONS" {
		return
	}

	if rhpAuthHeader, ok := req.Header["X-Rhp-Auth"]; ok {
		expectAuth := fmt.Sprintf("%d", time.Now().Unix()/10)
		if expectAuth == rhpAuthHeader[0] {
			log.Printf("valid refresh request from %v, running\n", req.RemoteAddr)
			loadUsers()
		}
	}
}

func websocketHandler(w http.ResponseWriter, req *http.Request) {
	wsConn, err := wsUpgrader.Upgrade(w, req, nil)

	if err != nil {
		log.Printf("websocketHandler upgrade failed: %v\n", err)
		return
	}

	okReqUUID, err := uuid.Parse(req.URL.RawQuery)

	if err != nil {
		log.Printf("bad ws query '%s'\n", req.URL.RawQuery)
		log.Println(req)
		return
	}

	gSubscribeHandler.Lock.Lock()
	defer gSubscribeHandler.Lock.Unlock()

	if pendingConn, ok := gSubscribeHandler.Pending[okReqUUID]; ok {
		if strings.Split(wsConn.RemoteAddr().String(), ":")[0] == strings.Split(pendingConn.Addr, ":")[0] {
			go registerNewClient(wsConn, pendingConn.Channel)
			delete(gSubscribeHandler.Pending, okReqUUID)
		} else {
			log.Printf("bad addr match %s vs %s\n", wsConn.RemoteAddr(), pendingConn.Addr)
		}
	} else {
		log.Printf("bad pending connection '%v'\n", okReqUUID)
	}
}

func parseJSON(path string, intoObj interface{}) error {
	file, err := os.Open(path)

	if err != nil {
		return err
	}

	defer file.Close()

	dec := json.NewDecoder(file)
	err = dec.Decode(intoObj)

	if err != nil {
		return err
	}

	return nil
}

func loadPlugin(path string) (*rhpPluginImpl, error) {
	ifaceType := reflect.TypeOf((*RhpPlugin)(nil)).Elem()
	pluginLoad, err := plugin.Open(path)

	if err != nil {
		return nil, err
	}

	// without stubbing the fields, reflect.ValueOf(...).Elem().FieldByName(...) below will return nil
	newPlugin := newRhpPluginImpl()

	// for each method declared in the interface, look for the same-named concrete defintion
	// in the loaded plugin. if that exists, find the field in the concrete implementation
	// instance (rhpPluginImpl) and set each function pointer accordingly
	for i := 0; i < ifaceType.NumMethod(); i++ {
		methodName := ifaceType.Method(i).Name
		pluginMethod, err := pluginLoad.Lookup(methodName)

		if err != nil {
			return nil, err
		}

		implValue := reflect.ValueOf(&newPlugin).Elem()

		if implValue.IsZero() {
			return nil, fmt.Errorf("unable to get value of concrete impl")
		}

		implElem := implValue.FieldByName(methodName)

		if implElem.IsZero() {
			return nil, fmt.Errorf("unable to set value on concrete impl")
		}

		// must .Convert to the target type (implElem.Interface()), else will panic with a strangely-worded error:
		// "reflect.Set: value of type T is not assignable to type T"
		// (not a typo: the 'from' and 'to' types in the error message will be exactly the same, because
		// indeed if we've made it this far the types will match, hence why .Convert() succeeds!)
		implElem.Set(reflect.ValueOf(pluginMethod).Convert(reflect.TypeOf(implElem.Interface())))
	}

	return &newPlugin, nil
}

func loadPlugins(fromPath string) (rhpPluginMapT, error) {
	retVal := rhpPluginMapT{}

	// we never return a non-nil error from within the walk function so as to allow .Walk() to continue;
	// there is the special return filepath.SkipDir, but it will cause Walk to skip remaining files,
	// which isn't what we want either. only in the case that `err` is already non-nil do we return non-nil.
	err := filepath.Walk(filepath.ToSlash(fromPath), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("filepath.Walk errored on entry: '%s' -> %v", path, err)
			return err
		}

		if filepath.Ext(path) != ".so" {
			return nil
		}

		log.Printf("found %s, checking for compatibility...", filepath.Base(path))
		newPlugin, err := loadPlugin(path)

		if err != nil {
			log.Printf("failed to load %s: %v", path, err)
			return nil
		}

		pBaseName := strings.Replace(filepath.Base(path), ".so", "", 1)
		log.Printf("loaded compatible plugin %s@%s", pBaseName, newPlugin.Version())
		retVal[pBaseName] = newPlugin

		return nil
	})

	return retVal, err
}

func loadUsers() {
	usersMapLock.Lock()
	defer usersMapLock.Unlock()

	err := parseJSON(g_usersFile, &usersMap)

	if err != nil {
		log.Panic(err.Error())
	}

	log.Printf("found %d valid users\n", len(usersMap))
}

func loadPublish(publishFile string, publishPrefix string, publishClient *redis.Client) (truePublishPrefix string, err error) {
	publishLock.Lock()
	defer publishLock.Unlock()

	err = parseJSON(publishFile, &publishMap)

	if err != nil {
		return
	}

	hostname, err := os.Hostname()

	if err != nil {
		return
	}

	truePublishPrefix = fmt.Sprintf("%s:%s", publishPrefix, hostname)

	var flippedChanMap map[string]string = map[string]string{}

	for chanName, mappedId := range publishMap {
		if mappedId == "" {
			mappedId = uuid.New().String()
			log.Printf("Generated ephemeral mapped ID %s for channel %s", mappedId, chanName)
		}

		flippedChanMap[mappedId] = fmt.Sprintf("%s::%s", truePublishPrefix, chanName)
	}

	publishMap = flippedChanMap

	if publishClient == nil {
		err = fmt.Errorf("bad publish client")
		return
	}

	go func() {
		for publishMsg := range publishChan {
			if channel, ok := publishMap[publishMsg.ChannelId]; ok {
				intRes := publishClient.Publish(channel, publishMsg.Message)

				if intRes == nil || intRes.Err() != nil {
					log.Printf("[%s] error publishing to %s: %v", publishMsg.User, channel, intRes.Err())
					continue
				}

				log.Printf("[%s] published to '%s', res=%d: %v", publishMsg.User, channel, intRes.Val(), publishMsg.Message)
			} else {
				log.Printf("[%s] bad channel ID %v", publishMsg.User, publishMsg.ChannelId)
			}
		}
	}()

	return
}

func main() {
	listenPort := flag.Uint("port", defaultListenPort, "http listen port")
	listenHost := flag.String("listen", defaultListenHost, "http listen host")
	redisPort := flag.Uint("redis-port", defaultRedisPort, "redis server port")
	redisHost := flag.String("redis-host", defaultRedisHost, "redis server host")
	pluginsPath := flag.String("plugins", defaultPluginsPath, "plugins path")
	usersFile := flag.String("users", defaultUsersFile, "users JSON file")
	publishFile := flag.String("publish", defaultPublishChannelsFile, "publish JSON file")
	publishPrefix := flag.String("publishPrefix", defaultPublishChannelPrefix, "publis channel name prefix")

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

	log.Printf("connected to redis://%s\n", redisOptions.Addr)
	redisDefaultClient = rc

	g_usersFile = *usersFile
	loadUsers()

	loadedPlugins.Lock.Lock()
	loadedPlugins.List, err = loadPlugins(*pluginsPath)
	loadedPlugins.Lock.Unlock()

	if err != nil {
		log.Printf("plugin load failed: %v", err)
	}

	gSubscribeHandler = new(subscribeHandler)

	truePublishPrefix, err := loadPublish(*publishFile, *publishPrefix, redisDefaultClient)

	if err == nil {
		log.Printf("publish mode loaded, no other functionality will be! prefix: %s", truePublishPrefix)
		http.Handle("/pub/", gSubscribeHandler)
	} else {
		http.Handle("/sub/", gSubscribeHandler)
		http.Handle("/list/", gSubscribeHandler)
		http.HandleFunc("/ws/sub", websocketHandler)
		http.HandleFunc("/refresh", refreshHandler)
	}

	listenSpec := fmt.Sprintf("%s:%d", *listenHost, *listenPort)
	log.Printf("listening on %s\n", listenSpec)

	http.ListenAndServe(listenSpec, nil)
}
