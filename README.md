
## datalab proxy

This datalab_proxy program provides a simple http api to manage datalab containers and proxies requests from users to the specific datalab backend.

API List:

### start instance
```
POST /datalab/start?ts=${timestamp}&filename=${filename}
HEADERS:
  X-SIGN: ${signature}
    signature = sha256(${sign_key} + ${timestamp} + ${filename})
BODY: ${file-content-in-octet-stream}
RESPONSE:
  { "code": "OK", "result": { "nonce": "a-nonce-string" } }

DESCRIPTION:
  Used by frontend to allocate a new datalab instance, use the returned ${nonce} 
  for the following requests. ${nonce} is expired in 10 minutes. ${nonce} is bind 
  with the newly created datalab instance, this information is stored in redis.
```

### view instance

```
GET /datalab/view/:nonce/*destination
RESPONSE:
  REDIRECT /*destination
DESCRIPTION:
  Used by frontend to embed this url for datalab access, browser will be REDIRECTed
  to /*destination .

  A cookie of ${session_id} will be written in browser, so that the proxy can know 
  which datalab instance is used to forward the browser request.

  The mapping information of ${session_id} to datalab instance is stored in redis.
```

### get static files from instance
```
GET /datalab/static/:nonce/*destination
RESPONSE:
  Content of the destination within the datalab instance binded with ${nonce}

DESCRIPTION:
  Used by frontend to fetch files from the binding datalab instance, e.g. *.png or *.csv.
  It does not redirect the '*destination' but fetch and return the content directly.
```

### proxy to assigned instance
```
ANY /*
DESCRIPTION:
  Any other request will be proxied to the given datalab instance. The proxy reads 
  ${session_id} in the cookie field of the browser request, and uses this this ${session_id} 
  to find the corresponding datalab instance and port, and then forward request to that 
  datalab instance, including http and websocket requests.
```
