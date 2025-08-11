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
