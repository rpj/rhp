package main

import (
	"net/url"
	"sync"
)

type RhpHandleListReqLookupFunc func(start int64, end int64) ([]string, error)

type RhpPlugin interface {
	Version() string
	HandleMsg(payload interface{}) (interface{}, error)
	HandleListReq(dir string, file string, query url.Values, listLookup RhpHandleListReqLookupFunc) (string, error)
}

type rhpPluginImpl struct {
	Version       func() string
	HandleMsg     func(interface{}) (interface{}, error)
	HandleListReq func(string, string, url.Values, RhpHandleListReqLookupFunc) (string, error)
}

// newRhpPluginImpl is defined here so as to make it easy to keep in-sync
// with the definition of rhpPluginImpl above
func newRhpPluginImpl() rhpPluginImpl {
	return rhpPluginImpl{
		func() string { return "" },
		func(_ interface{}) (interface{}, error) { return nil, nil },
		func(_ string, _ string, _ url.Values, _ RhpHandleListReqLookupFunc) (string, error) { return "", nil },
	}
}

type rhpPluginMapT map[string]*rhpPluginImpl

type rhpPluginsT struct {
	Lock sync.Mutex
	List rhpPluginMapT
}
