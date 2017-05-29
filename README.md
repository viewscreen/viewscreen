# Trickle Cloud - open source self-hosted remote torrent client, written in Go

![Screenshot - Downloads](https://raw.githubusercontent.com/tricklecloud/trickle/master/screenshot1.png)

![Screenshot - Friends](https://raw.githubusercontent.com/tricklecloud/trickle/master/screenshot2.png)

## Features

* **Share downloads with friends**
  * View all your friends’ shared downloads
  * High speed server-to-server transfer
* **Stream media on any device**
  * Transcode files to .mp4 (H264/AAC)
  * Web-based remote client
  * Nothing to download or install
  * Files are stored and downloaded on your server
* **Mobile friendly web interface for torrenting**
  * Private podcast URL for mobile devices (offline downloads)
  * Save files from your server to your computer
* **Simple self-hosting**
  * Public Docker image
  * Single Go binary
  * Automatic TLS using Let's Encrypt
  * Redirects http to https

## Running

### 1. Get a server

**Recommended Specs**

* Type: VPS or dedicated
* Distribution: Ubuntu 16.04 (Xenial)
* Memory: 1GB+
* Storage: 20GB+

**Recommended Providers**

* [OVH](https://www.ovh.com/)
* [Scaleway](https://www.scaleway.com/)

### 2. Add a DNS record

Create a DNS `A` record in your domain pointing to your server's IP address.

**Example:** `trickle.example.com  A  172.16.1.1`

### 3. Enable Let's Encrypt

When enabled with the `--letsencrypt` flag, trickle runs a TLS ("SSL") https server on port 443. It also runs a standard web server on port 80 to redirect clients to the secure server.

**Requirements**

* Your server must have a publicly resolvable DNS record.
* Your server must be reachable over the internet on ports 80 and 443.

### 4. Run as a Docker container

The official image is `tricklecloud/trickle`, which should run in any up-to-date Docker environment.

Follow the official Docker install instructions: [Get Docker CE for Ubuntu](https://docs.docker.com/engine/installation/linux/docker-ce/ubuntu/)

```bash

# Your download directory should be bind-mounted as `/data` inside the container using the `--volume` flag.
$ mkdir /home/<username>/Downloads

$ sudo docker create                            \
    --name trickle --init --restart always      \
    --publish 80:80 --publish 443:443           \
    --volume /home/<username>/Downloads:/data   \
    tricklecloud/trickle:latest --letsencrypt --http-host trickle.example.com

$ sudo docker start trickle

$ sudo docker logs -f trickle
time="2027-01-19T00:00:00Z" level=info msg="Trickle URL: https://trickle:924433342@trickle.example.com/trickle"

```

### 5. Updating the container image

Pull the latest image, remove the container, and re-create the container as explained above.

```bash
# Pull the latest image
$ sudo docker pull tricklecloud/trickle

# Stop the container
$ sudo docker stop trickle

# Remove the container (data is stored on the mounted volume)
$ sudo docker rm trickle

# Re-create and start the container
$ sudo docker create ... (see above)

```



### Usage

**Example usage:**

```bash
$ trickle --letsencrypt --http-host trickle.example.com --download-dir /home/ubuntu/Downloads
```

```bash
$ trickle --help
Usage of trickle:
  -backlink string
        backlink (optional)
  -debug
        debug mode
  -download-dir string
        download directory (default "/data")
  -http-addr string
        listen address (default ":80")
  -http-host string
        HTTP host
  -http-prefix string
        HTTP URL prefix (not supported yet) (default "/trickle")
  -letsencrypt
        enable TLS using Lets Encrypt
  -metadata
        use metadata service
  -reverse-proxy-header string
        reverse proxy auth header (default "X-Authenticated-User")
  -reverse-proxy-ip string
        reverse proxy auth IP
  -torrent-addr string
        listen address for torrent client (default ":61337")

```


### Standalone

```bash

# Install ffmpeg.
$ sudo add-apt-repository -y ppa:jonathonf/ffmpeg-3
$ sudo apt-get update
$ sudo apt-get install -y wget ffmpeg x264

# Download the trickle binary.
$ sudo wget -O /usr/bin/trickle \
    https://github.com/tricklecloud/trickle/blob/master/trickle-linux-amd64

# Make it executable.
$ sudo chmod +x /usr/bin/trickle

# Allow it to bind to privileged ports 80 and 443.
$ sudo setcap cap_net_bind_service=+ep /usr/bin/trickle

# Enable Let's Encrypt using your domain for automatic TLS configuration.
$ trickle --letsencrypt --http-host trickle.example.com --download-dir /home/ubuntu/Downloads
INFO[0000] Trickle URL: https://trickle:924433342@domain/trickle

```

#### Using screen to run in debug mode

If you're having problems, it might help to run trickle in a screen session with debug logging enabled.

``` bash
# Install screen
$ screen || sudo apt-get install -y screen

# Launch in a detached screen session.
$ screen -S trickle -d -m trickle --debug --letsencrypt --http-host <your domain name>

# List all screen sessions.
$ screen -ls

# Attach to the running session.
$ screen -r trickle

# Press ctrl-a + ctrl-d to detach.
```


### Building

The easiest way to build the static binary is using the `Dockerfile.build` file. You can also build a docker image for running the binary.

```bash
# Download the git repo.
$ git clone https://github.com/tricklecloud/trickle.git
$ cd trickle/

# Compile the code and create a Docker image for it.
$ sudo docker build --build-arg TRICKLE_VERSION=$(git rev-parse --short HEAD) -t trickle:build -f Dockerfile.build .

# Create a container based on the image we just built.
$ sudo docker create --name tricklebuild trickle:build

# Extract the binary from the image.
$ sudo docker cp tricklebuild:/usr/bin/trickle-linux-amd64 trickle-linux-amd64

# We're done with the build container.
$ sudo docker rm trickelbuild

# Inspect the binary.
$ file trickle-linux-amd64
trickle-linux-amd64: ELF 64-bit LSB  executable, x86-64, version 1 (GNU/Linux), statically linked, for GNU/Linux 2.6.32, BuildID[sha1]=c2a6f5a9e12c8c35117ec52c3572bf844c510957, stripped

# Run the binary.
$ ./trickle-linux-amd64 --help

# Build a tiny alpine "runner" image.
$ sudo docker build -t trickle:latest .
```

### Thanks

Thanks to all the projects and developers that made this project possible.

* The free certificate for your server comes from [Let's Encrypt](https://letsencrypt.org/), which is doing a lot of good in the world. Get your company to sponsor them!
