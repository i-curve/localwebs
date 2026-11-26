# Localwebs

[![Docker workflow](https://github.com/i-curve/localwebs/actions/workflows/docker.yml/badge.svg)](https://github.com/i-curve/localwebs/actions/workflows/docker.yml)
[![Docker Hub](https://img.shields.io/badge/Docker%20Hub-icurve%2Flocalwebs-2496ED?logo=docker&logoColor=white)](https://hub.docker.com/r/icurve/localwebs)
[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)

[English](README.md) | 简体中文

## 概述

Localwebs（Local Web Send）是一个基于浏览器的文件和剪贴板分享工具，无需注册账号即可在设备之间传输内容。

## 功能特性

- 使用 6 位数字配对码连接设备，接收方确认后才开始传输。
- 支持发送文件、文件夹，以及浏览器允许粘贴的文本、链接、图片和文件。
- 单个文件原样发送；多个文件或文件夹打包为 ZIP，空目录不包含在内。每次传输上限为 **100 GB**，包含 ZIP 打包开销；剪贴板文本上限为 **1 MiB**。
- 支持连接多个实例，让不同节点上的设备互传。
- 传输页和管理后台支持简体中文和英文。

## 安装/运行方式

### Docker

使用 [Docker Hub 镜像](https://hub.docker.com/r/icurve/localwebs)运行，无须克隆仓库：

```sh
docker pull icurve/localwebs:latest
export LOCALWEBS_USERNAME=admin
# 请先在环境变量 LOCALWEBS_PASSWORD 中设置强密码。
docker run -d --name localwebs --restart unless-stopped -p 8080:8080 \
  -e LOCALWEBS_USERNAME -e LOCALWEBS_PASSWORD \
  -v localwebs-data:/data icurve/localwebs:latest
```

如需更改宿主机端口，例如使用 `8081`，将 `-p 8080:8080` 改为 `-p 8081:8080`。数据卷 `localwebs-data` 用于保留管理后台设置，请勿在更新时删除。环境变量可通过 Docker 容器配置查看，请妥善保护宿主机访问权限。

查看日志使用 `docker logs -f localwebs`。更新镜像时，先执行 `docker pull icurve/localwebs:latest`，再执行 `docker stop localwebs` 和 `docker rm localwebs`，最后重新运行上述 `docker run` 命令。重新运行前请确认管理员环境变量仍已设置；删除容器不会删除命名数据卷。

也可以在克隆后的项目目录执行 `docker build -t localwebs .`，然后将 `docker run` 命令末尾的镜像名替换为 `localwebs`。

### 从源码运行

需要 Go 1.24 或更高版本及 Node.js 22。克隆仓库后，在项目目录下执行：

```sh
npm ci
npm run build
go run .
```

启动后在本机访问 `http://localhost:8080`，其他设备使用宿主机地址访问。输入相同的 6 位配对码（保留开头的 `0`），选择内容并点击接收设备，接收方确认后即可下载或复制。请使用随机配对码，仅分享给接收方，并在传输期间保持双方页面打开。

## 相关配置

访问 `/admin` 管理账号和集群。默认管理员用户名为 `admin`；若未设置密码，后台无法登录，但不影响文件传输。可通过 `LOCALWEBS_USERNAME` 和 `LOCALWEBS_PASSWORD` 设置管理员账号。Docker 中使用数据卷保存后台修改；从源码运行时默认保存到项目目录的 `config.json`。

后台保存的设置优先于环境变量，环境变量优先于系统默认值。监听地址可通过 `LOCALWEBS_ADDR` 设置，默认为 `:8080`。在不受信任的网络上请使用 HTTPS。

### 集群

集群默认关闭。连接多个实例时：

1. 登录各实例的 `/admin`，启用 **集群**。
2. 在每个节点设置相同的强密钥：24–256 个不含空格的可打印 ASCII 字符。
3. 逐行填写可访问的节点 HTTP(S) 地址并保存；每条连接只需一边填写对方的地址。例如，在 B 上填写 A 和 C 的地址，A 和 C 无须互相填写。
4. 节点连通后，在任意已连接节点输入相同的配对码即可互传；例如 A 与 C 上的用户可以通过 B 互传。

## 许可证

本项目采用 [BSD 3-Clause License](LICENSE)。
