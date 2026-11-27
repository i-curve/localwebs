# Localwebs

[![Docker workflow](https://github.com/i-curve/localwebs/actions/workflows/docker.yml/badge.svg)](https://github.com/i-curve/localwebs/actions/workflows/docker.yml)
[![Docker Hub](https://img.shields.io/badge/Docker%20Hub-icurve%2Flocalwebs-2496ED?logo=docker&logoColor=white)](https://hub.docker.com/r/icurve/localwebs)
[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)

English | [简体中文](README.zh-CN.md)

## Overview

Localwebs (Local Web Send) is a browser-based tool for sharing files and clipboard contents between devices without an account.

## Features

- Connect devices with a six-digit pairing code; files require the recipient's acceptance, while clipboard text is displayed directly.
- Send files, folders, and text, links, images, or files that the browser allows you to paste.
- Single files are sent as-is; multiple files and folders are packaged as a ZIP, excluding empty directories. Each transfer is limited to **100 GB**, including ZIP overhead; clipboard text is limited to **1 MiB**.
- Use clustering to transfer between devices on different nodes.
- Switch the transfer and admin pages between English and Simplified Chinese.

## Installation and usage

### Docker

Run the [Docker Hub image](https://hub.docker.com/r/icurve/localwebs) without cloning the repository:

```sh
docker pull icurve/localwebs:latest
export LOCALWEBS_USERNAME=admin
# Set LOCALWEBS_PASSWORD in your environment to a strong private password first.
docker run -d --name localwebs --restart unless-stopped -p 8080:8080 \
  -e LOCALWEBS_USERNAME -e LOCALWEBS_PASSWORD \
  -v localwebs-data:/data icurve/localwebs:latest
```

To change the host port, for example to `8081`, replace `-p 8080:8080` with `-p 8081:8080`. The `localwebs-data` volume retains admin settings; do not delete it during updates. Environment variables can be viewed through the Docker container configuration, so restrict access to the host.

Use `docker logs -f localwebs` to view logs. To update, run `docker pull icurve/localwebs:latest`, then `docker stop localwebs` and `docker rm localwebs`, and repeat the `docker run` command above. Make sure the administrator environment variables are still set before running it again; removing the container does not delete the named volume.

Alternatively, run `docker build -t localwebs .` from a cloned repository and replace the image name at the end of the `docker run` command with `localwebs`.

### Download and run

Download the archive for your operating system and architecture from [GitHub Releases](https://github.com/i-curve/localwebs/releases). Extract it and run `./localwebs` from that directory, or `.\localwebs.exe` in PowerShell on Windows. Keep the included `dist` directory alongside the executable.

### Run from source

Go 1.24 or newer and Node.js 22 are required. After cloning the repository, run:

```sh
npm ci
npm run build
go run .
```

Open `http://localhost:8080` on the host, or use the host's address from another device. Enter the same six-digit code on both devices, including any leading zeroes, select the content, and click the receiving device. The recipient accepts files before downloading them and can copy clipboard text directly. Use a random code, share it only with recipients, and keep both pages open during transfer.

## Configuration

Visit `/admin` to manage the account and cluster. The default administrator username is `admin`; if no password is set, admin login is disabled but transfers still work. Set the credentials with `LOCALWEBS_USERNAME` and `LOCALWEBS_PASSWORD`. Docker uses a data volume to retain settings saved in the admin page; other installation methods save settings to `config.json` in the current directory by default.

Settings saved in the admin page take precedence over environment variables, which take precedence over the built-in defaults. Set `LOCALWEBS_ADDR` to change the listening address, which defaults to `:8080`. Use HTTPS on untrusted networks.

### Cluster

Clustering is disabled by default. To connect multiple instances:

1. Sign in at `/admin` on each instance and enable **Cluster**.
2. Set the same strong shared key on every node: 24–256 printable ASCII characters without spaces.
3. Enter reachable HTTP(S) peer addresses, one per line, and save. Only one side of each link needs the other's address. For example, enter both A and C's addresses on B; A and C do not need to configure each other.
4. Once linked, users can enter the same pairing code on any connected node. In the example, users on A and C can transfer through B.

## License

This project is licensed under the [BSD 3-Clause License](LICENSE).
