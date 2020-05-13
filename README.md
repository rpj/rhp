# Redis-to-HTTP proxy

Currently the only implemented functionality is proxying of pub/sub subscriptions onto a websocket, as follows:

* specify `channel` in path to `GET` request `/sub/[channel]`, providing valid credentials as Basic authorization
  * credentials are matched against those stored in `users.json` - make sure this is `chmod`'ed to `0600`!
* if `/sub/...` request was valid & authorized, it will return a *one-time* token `SUB_TOKEN`
* use the token as the sole query parameter to the websocket endpoint to start receiving data, e.g. `/ws/sub?SUB_TOKEN`

The entirety of the aforementioned procedure can be perfomed on the CLI via `wscat` and `curl`:

```
wscat -c ws://HOST:PORT/ws/sub?`curl -H "Authorization: Basic B64_CRED" http://HOST:PORT/sub/CHANNEL`
```

where `HOST`, `PORT`, `B64_CRED` & `CHANNEL` should be replaced with appropriate values.

An example client can be found [here](https://github.com/rpj/rhp-client-example), a browser-based realtime display of [RPJiOS](https://github.com/rpj/rpi) device sensor data.
