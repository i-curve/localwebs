# Localwebs

[简体中文](README.zh-CN.md)

Localwebs (Local Web Send) sends files, folders, and clipboard contents between browsers. No account is needed to send or receive. Both the transfer page and the admin page can be switched between English and Simplified Chinese.

## Send files

1. Open Localwebs on both devices. Enter the same **six-digit code**, including any leading zeroes.
2. Click **Choose files** or **Choose folder**, then click the receiving device.
3. On the other device, choose **Accept and download** to start the browser download. Nothing is sent before acceptance.

Use a random code and share it only with your recipients. Keep both tabs open during transfer. Single files are sent as-is; folders and multiple files arrive as a ZIP. Empty directories are not included. The limit is **100 GB** per transfer, including any ZIP overhead.

To share your clipboard, paste text, a link, an image, or files into **Clipboard**, then click the receiving device. The receiver can accept and copy text (up to **1 MiB**) or download pasted images and files that the browser makes available.

## Run with Docker

From the project directory:

```sh
docker build -t localwebs .
export LOCALWEBS_USERNAME=admin
# Set LOCALWEBS_PASSWORD in your environment to a strong private password first.
docker run -d --name localwebs -p 8080:8080 \
  -e LOCALWEBS_USERNAME -e LOCALWEBS_PASSWORD \
  -v localwebs-data:/data localwebs
```

Open `http://localhost:8080`, or use the host machine's address from another device. Sign in at `/admin` to manage the account and cluster. If no password was configured, admin login is disabled; file transfers still work. Keep the data volume to retain changes. Settings saved in `/admin` take precedence over environment variables, which take precedence over the built-in defaults. If editing `config.json` directly, use `username`, `password`, and the `clusterPeerUrls` list. Use HTTPS on untrusted networks.

## Connect multiple installations

1. Sign in at `/admin` on each installation and enable **Cluster**. It is off by default.
2. Set the same strong shared key (24 or more characters, no spaces) on every node.
3. Enter reachable peer addresses, one per line, and save. Only one side of each link needs the other's address. For example, enter both A and C's addresses on B; A and C do not need to configure each other.
4. Once the nodes are linked, users can enter the same pairing code on any connected node. In the example, users on A and C can transfer through B.

## Publish to Docker Hub

Set the repository's `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` GitHub Actions secrets. The workflow publishes `<username>/localwebs` by default; set the `DOCKERHUB_IMAGE` repository variable to use another image name. A push to the default branch updates `latest`, and a `v*` tag publishes a versioned image. To use a published image, replace `localwebs` at the end of the `docker run` command with `<username>/localwebs:latest`.
