# Redis-to-HTTP proxy

Currently the only implemented functionality is proxying of pub/sub subscriptions onto a websocket, as follows:

* specify channel in path to `GET` request `/sub/[channel]`, providing valid credentials as Basic authorization
  * credentials are matched against those stored in `users.json` - make sure this is `chmod`'ed to `0600`!
* if `/sub/...` request was valid and authorized, it will return a UUID token `SUB_UUID`
* use the UUID token as the sole query parameter to the websocket endpoint to start receiving data:
  * `ws://host:port/ws/sub?SUB_UUID`

The entirety of the aforementioned procedure can be perfomed on the CLI via `wscat` and `curl`:

```
  wscat -c ws://HOST:PORT/ws/sub?`curl -v -H "Authorization: Basic BASE64_USER_PWD" http://HOST:PORT/sub/CHANNEL`
```

where `HOST`, `PORT`, `BASE64_USER_PWD` & `CHANNEL` should be replaced with appropriate values.
