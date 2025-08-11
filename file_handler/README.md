```
POST /sync
Content-Type: application/json
{
    "path/to/file": "base-64-encoded-file",
    "path/to/file2": "base-64-encoded-file2",
}
```

```bash
$ echo "hello gaga" > /tmp/a.txt
$ curl -X POST http://localhost:8000/sync \
   -H "Content-Type: application/json" \
   -d '{"/tmp/b/b.txt": "'$(base64 -w 0 /tmp/a.txt)'", "/tmp/b/b2.txt": "'$(base64 -w 0 /tmp/a.txt)'"}'
$ cat /tmp/b/b.txt
```


```bash
$ curl -X POST http://localhost:8080/sync \
   -H "Content-Type: application/json" \
   -d '{"/app/applet/app/api/hello/route.js": "'$(base64 -w 0 ./applet/app/api/hello/route.js)'"}'

$ curl -X POST http://localhost:8080/sync \
   -H "Content-Type: application/json" \
   -d '{"/app/applet/app/page.js": "'$(base64 -w 0 ./applet/app/page.js)'"}'

```

### Optional: Prewarm after sync

You can ask the container to prewarm specific paths immediately after syncing files by adding query parameters to the `/sync` request:

```
POST /sync?prewarm=true&prewarmPaths=/,/api/hello&port=3000&wait=false
```

- `prewarm`: set to `true` to trigger prewarming in the background.
- `prewarmPaths`: either a comma-separated list or a JSON array (e.g., `["/", "/api/hello"]`).
- `port`: app port to target (defaults to 3000 via `APP_PORT`).
- `wait`: set to `true` to wait for prewarm to complete before returning the response (default: false).

Examples:

```bash
curl -X POST 'http://localhost:8080/sync?prewarm=true&prewarmPaths=/,/api/hello' \
  -H 'Content-Type: application/json' \
  -d '{"/app/applet/app/page.js": "'$(base64 -w 0 ./applet/app/page.js)'"}'

# Using JSON array and waiting for prewarm
curl -X POST 'http://localhost:8080/sync?prewarm=true&prewarmPaths=["/","/api/hello"]&wait=true' \
  -H 'Content-Type: application/json' \
  -d '{"/app/applet/app/page.js": "'$(base64 -w 0 ./applet/app/page.js)'"}'
```
