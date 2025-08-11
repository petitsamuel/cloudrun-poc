Running the container:

```
docker build -t my-ais-container .
docker run -d -p 8080:8080 --name my-ais-instance my-ais-container
```


Get logs:

```
sudo docker logs my-ais-instance

```



Get to the container files

```
docker exec -it hot-reload /bin/bash
```


Update package.json locally:

```
cat <<EOF >> package.json
{
  "name": "hot-reload-app",
  "version": "0.1.0",
  "private": true,
  "scripts": {
    "dev": "next dev",
    "build": "next build",
    "start": "next start",
    "lint": "next lint"
  },
  "dependencies": {
    "react": "^18",
    "react-dom": "^18",
    "next": "14.2.3",
    "next-auth": "4.24.11"
  }
}
EOF
```
