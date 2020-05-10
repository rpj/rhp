package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

var hostname string

func RhpPlugin(payload interface{}) (interface{}, error) {
	rxTime := float64(time.Now().UnixNano()) / 1e9

	if len(hostname) == 0 {
		hn, err := os.Hostname()

		if err != nil {
			log.Panic("can't get hostname!")
		}

		hostname = fmt.Sprintf("rhp://%s", hn)
	}

	var parsed map[string]interface{}

	err := json.Unmarshal([]byte(payload.(string)), &parsed)

	if err != nil {
		return payload, fmt.Errorf("unmarshal")
	}

	if ds, ok := parsed["__ds"]; ok {
		parsed["__ds"] = map[string]interface{}{
			"host":    hostname,
			"prev":    ds,
			"rate":    1,
			"tsDelta": rxTime - ds.(map[string]interface{})["ts"].(float64),
			"ts":      rxTime,
		}

		newPayload, err := json.Marshal(parsed)

		if err != nil {
			return payload, fmt.Errorf("marshal")
		}

		return string(newPayload), nil
	}

	return payload, nil
}
