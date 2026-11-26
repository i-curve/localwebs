# Localwebs

[English](README.md)

Localwebs（Local Web Send）可以在浏览器之间互传文件、文件夹和剪贴板内容，不需要账号。传输页和管理后台都可以切换简体中文或英文。

## 收发文件

1. 在两台设备上打开 Localwebs，输入相同的 **6 位数字配对码**，保留开头的 `0`。
2. 点击 **选择文件** 或 **选择文件夹**，选好后点击接收设备。
3. 接收方点击 **接受并下载** 后，浏览器才会开始下载；确认前不会发送内容。

请使用随机配对码，只分享给接收方。传输期间保持双方页面打开。单个文件原样发送，文件夹和多个文件打包为 ZIP，空目录不包含在内。每次传输上限为 **100 GB**，包含 ZIP 打包开销。

分享剪贴板时，将文本、链接、图片或文件粘贴到 **剪贴板** 输入框，再点击接收设备。接收方确认后可以复制文本（最多 **1 MiB**），或下载浏览器允许粘贴的图片和文件。

## 用 Docker 运行

在项目目录下执行：

```sh
docker build -t localwebs .
export LOCALWEBS_USERNAME=admin
# 请先在环境变量 LOCALWEBS_PASSWORD 中设置强密码。
docker run -d --name localwebs -p 8080:8080 \
  -e LOCALWEBS_USERNAME -e LOCALWEBS_PASSWORD \
  -v localwebs-data:/data localwebs
```

本机访问 `http://localhost:8080`；其他设备请使用宿主机的地址。访问 `/admin` 管理账号和集群。若未设置管理员密码，后台无法登录，但不影响文件传输。保留数据卷即可保留修改。后台保存的设置优先于环境变量，环境变量优先于系统默认值。手动修改 `config.json` 时，使用 `username`、`password` 和 `clusterPeerUrls` 地址列表。不受信任的网络上请使用 HTTPS。

## 连接多个实例

1. 分别登录各个实例的 `/admin`，勾选 **启用集群**。该功能默认关闭。
2. 每个节点填写相同的强密钥（至少 24 个字符，不含空格）。
3. 将可访问的节点地址逐行填入并保存。每条连接只需一边填写对方的地址。例如，在 B 上填写 A 和 C 的地址，A 和 C 无须互相填写。
4. 节点连通后，用户在任一节点输入相同的配对码即可互传。例如 A 与 C 上的用户可以通过 B 互传。

## 发布到 Docker Hub

在仓库的 GitHub Actions 中配置 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN` 两个密钥。默认发布到 `<用户名>/localwebs`，可用仓库变量 `DOCKERHUB_IMAGE` 指定其他镜像名。推送到默认分支会更新 `latest`，推送 `v*` 标签会发布版本镜像。使用已发布镜像时，将 `docker run` 命令末尾的 `localwebs` 替换为 `<用户名>/localwebs:latest`。
