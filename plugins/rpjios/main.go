package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"
)

var hostname string

func Version() string {
	return "0.0.2"
}

func HandleMsg(payload interface{}) (interface{}, error) {
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

func HandleListReq(dir string, file string, query url.Values, listLookup RhpHandleListReqLookupFunc) (string, error) {
	if dir != "/list/" || file == "" {
		return "", nil
	}

	if timeSpec, ok := query["back"]; ok {
		back, err := strconv.ParseInt(timeSpec[0], 10, 64)

		if err == nil {
			start := int64(0)
			end := int64(100)
			_ux := time.Now()
			limUnix := _ux.Unix() - (back * 60)
			log.Printf("NOW %v (%d)\n", _ux.String(), _ux.Unix())
			log.Printf("LIM %v (%d)\n", time.Unix(limUnix, 0), limUnix)
			retList := make([][2]float64, 0, end-start)
			stillGoing := true

			for stillGoing {
				list, err := listLookup(start, end)

				if err != nil {
					stillGoing = false
					log.Printf("broke! %v", err)
					break
				}

				if len(list) == 0 {
					stillGoing = false
					break
				}

				for _, toDec := range list {
					var newVal [2]float64
					err = json.Unmarshal([]byte(toDec), &newVal)

					if err != nil {
						log.Printf("bad rpjiosListVal! %v\n", err)
						stillGoing = false
						break
					}

					if int64(newVal[0]) >= limUnix {
						retList = append(retList, newVal)
					} else {
						stillGoing = false
					}
				}

				if stillGoing {
					start = end
					end = end + end
				}
			}

			log.Printf("1 RETLIST LEN: %v (%v)\n", len(retList), cap(retList))

			if cadSpec, ok := query["cad"]; ok {
				cad, err := strconv.ParseInt(cadSpec[0], 10, 64)

				if err == nil && cad < (back*60) {
					newList := make([][2]float64, 0, int64(cap(retList))/cad)
					lastMark := int64(-1)

					// TODO: detect natural cadence and then jump over N elements
					// in retList as optimization
					for _, chkVal := range retList {
						curMark := int64(chkVal[0])

						if lastMark == int64(-1) || lastMark-curMark > cad {
							newList = append(newList, chkVal)
							lastMark = curMark
						}
					}

					retList = newList
					log.Printf("2 RETLIST LEN: %v (%v)\n", len(retList), cap(retList))
				}
			}

			rlStr, err := json.Marshal(retList)

			if err == nil {
				return string(rlStr), nil
			}
		}
	}

	return "", nil
}
