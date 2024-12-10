# Localsetup

- Build the binary<br>
```go build -0 main main.go```

- Open ```app/index.html``` and copy the sdp

- Paste the sdp and start the binary <br>
```echo <sdp> | ./main```

- Copy back the sdp generated in the stdout and paste it back in the index.html

- Now the hls stream is started in `http://localhost:8080` which you can listen with vlc
