# 🚗 用车账单生成器（Go + SQLite + Docker） v1.0.0

一个帮你在微信里快速生成「用车账单 / 对外报价」的小工具：

- ✅ 接送机 / 包车 / 点对点 三种账单，中英文报价
- ✅ 用车类型用**顶部标签页**切换（手机友好）
- ✅ 行程路线：地址间用**空格分隔自动转 →**（如：梦工厂 → 梦工厂A区 → 宝马家居）
- ✅ 对接人自动加 **@** 前缀，像微信 @ 一样
- ✅ 前台「**一键复制账单**」到系统剪贴板（iPhone Safari 也支持）
- ✅ 「**保存到记账本**」：核对无误后一键写入数据库
- ✅ 总金额自动从结果文本解析（两台车合并账单也能取对），另有**实收金额**字段
- ✅ 后台「**记账管理**」：数据库记账（公司 / 日期 / 总金额 / 实收 / 对接人 / 账单内容 / 是否收钱）
- ✅ 收钱状态切换带二次确认
- ✅ 按筛选结果导出 **Excel(.xlsx) / CSV**，WPS / Excel 可直接打开
- ✅ 后台手机 / 电脑自适应
- ✅ 后台管理多家合作公司的费率（增删改）
- ✅ 单文件 Go 程序 + Docker + SQLite，部署超简单
- ✅ 费率存 `data/firms.json`，订单存 `data/orders.db`（SQLite），备份两个文件即可

---

## 📁 目录结构

```
car-billing-go/
├── main.go              # Go 主程序（核心代码）
├── go.mod / go.sum      # Go 模块配置与依赖锁
├── Dockerfile           # 镜像构建文件
├── docker-compose.yml   # 标准部署配置（命令行用）
├── deploy.sh            # 命令行一键部署脚本
├── .gitignore           # git 忽略规则
├── .github/             # （可选）GitHub Actions 自动构建镜像
├── templates/           # 页面模板（可自行改样式）
│   ├── index.html       # 账单生成首页（复制+保存记账）
│   ├── login.html       # 后台登录页
│   └── admin.html       # 费率配置 + 记账管理 后台
└── data/
    ├── firms.json       # 费率数据（重要！备份）
    └── orders.db        # 记账数据库（首次运行自动生成，重要！备份）
```

---

## 📖 部署教程（Arcane 面板版，推荐）

> 你只需要：**① 把代码放到 GitHub → ② Arcane 面板里填一份 compose → ③ 完成**
> 全程不用 SSH、不用装 Git、不用镜像仓库。

### 第一步：把项目放到 GitHub

1. 打开 GitHub，点右上角 **+** → **New repository**
2. Repository name 填：`car-billing-go`，可见性选 **Public** 或 **Private**，其他不勾，点 **Create repository**
3. 在仓库页面点 **Add file → Upload files**
4. 打开本机的 `car-billing-go` 文件夹，全选（Ctrl+A），把**所有文件和文件夹**一起拖进上传框，点 **Commit changes**
   > `templates`、`data` 是文件夹，要一起拖进去。

### 第二步：Arcane 面板部署

1. 打开你的 Arcane 面板 → **Projects / 项目** → **新建项目 / New Project**
2. 创建方式选「**Compose 内容 / 粘贴 YAML**」，把下面这份完整粘贴进去：

```yaml
services:
  car-billing:
    build: https://github.com/你的用户名/car-billing-go.git
    container_name: car-billing
    restart: unless-stopped
    ports:
      - "3333:8080"
    environment:
      ADMIN_PASSWORD: "123456"
    volumes:
      - car-billing-data:/app/data
volumes:
  car-billing-data:
```

> 🔑 把 `你的用户名` 换成你的 GitHub 用户名。
> `build: https://github.com/...git` 是 Docker 的 Git 构建语法，部署时自动从 GitHub 拉代码构建，**以后你更新代码推上 GitHub，面板里点重建就能更新**，数据卷 `car-billing-data` 保留，数据不丢。

3. 点 **部署 / Deploy / Up**，等构建完成（首次要下载依赖，2-5 分钟）
4. 云服务器安全组放行 **3333** 端口

### 第三步：访问使用

| 页面 | 地址 | 说明 |
|------|------|------|
| 首页 | `http://你的VPS的IP:3333` | 生成账单（一键复制 / 保存到记账本） |
| 后台 | `http://你的VPS的IP:3333/admin` | 费率配置 + 记账管理，**初始密码：123456** |

> ⚠️ 第一次用建议立刻改密码：部署后编辑项目 compose 里的 `ADMIN_PASSWORD`，保存重新部署。

---

## 📖 部署教程（命令行版，备用）

```bash
# 1. 连上 VPS（宝塔终端 / SSH）
# 2. 下载代码
cd /root
git clone https://github.com/你的用户名/car-billing-go.git
cd car-billing-go
# 3. 启动
docker compose up -d --build
# 4. 放行 3333 端口即可访问
```

---

## 🔄 版本发布流程（以后升级用）

> 当前版本是 **v1.0.0（首版）**。以后每次升级：**改代码 → 推 GitHub → Arcane 点重建**。
> 想保留旧版本测试再切换时，用「新建项目 + 新端口 + 新数据卷」的方式并行跑。

### 日常小更新（不保留旧版）

1. 改代码 → push 到 GitHub
2. Arcane 面板 → 对应项目 → 点「重新构建 / 重建」
3. 自动拉最新代码重建容器，数据卷保留、订单不丢

### 想先测试再切换（新旧共存）

1. **Arcane 新建一个项目**，compose 粘贴下面这份（注意：端口 3334、容器名、数据卷都不同，和线上 3333 完全隔离）：

```yaml
services:
  car-billing-new:
    build: https://github.com/你的用户名/car-billing-go.git
    container_name: car-billing-new
    restart: unless-stopped
    ports:
      - "3334:8080"
    environment:
      ADMIN_PASSWORD: "123456"
    volumes:
      - car-billing-new-data:/app/data
volumes:
  car-billing-new-data:
```

2. 部署后访问 `http://IP:3334` 测试新版本（线上 3333 不受影响）
3. 测试通过：停掉旧项目容器 → 把新项目 compose 改回端口 3333、容器名 `car-billing`、数据卷 `car-billing-data` → 重新部署
4. 回滚：有问题就把旧容器启动回来即可

### 打版本标签（建议）

GitHub 网页 → 仓库右侧 **Releases → Create a new release** → Tag 填 `v1.0.0` → Publish。
以后升级打 v1.1.0、v2.0.0…… 每个版本在 GitHub 上永久留档。

---

## 🛠 常用命令（命令行部署时用）

```bash
# 看运行日志（排查问题用）
docker logs -f car-billing

# 停止服务
docker compose down

# 重启服务
docker compose restart

# 备份费率数据（重要！）
cp data/firms.json ~/firms-备份-$(date +%F).json

# 备份记账数据（重要！）
cp data/orders.db ~/orders-备份-$(date +%F).db
```

---

## ❓ 常见问题

**Q：打不开网页？**
检查两步：① 云服务器安全组是否放行 3333 端口；② VPS 防火墙是否放行 3333 端口。

**Q：想换访问端口？**
改 compose 里 `"3333:8080"` 左边的数字，比如 `"9000:8080"`，然后重新部署。

**Q：数据会丢吗？**
不会。`car-billing-data` 是命名数据卷，容器删了重建数据还在。建议定期备份 `firms.json` 和 `orders.db`。

**Q：想在手机上用？**
部署好后手机浏览器直接打开 `http://IP:3333`（页面手机自适应），可添加到主屏幕当 App 用。

**Q：更新后提示数据表错误？**
不会发生。v1.0.0 起程序启动时会自动检测并补齐缺失的列，旧数据自动迁移。

---

## ⚙️ 环境变量说明

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `8080` | 程序监听端口（容器内，一般不用改） |
| `ADMIN_PASSWORD` | `123456` | 后台密码 |
| `SESSION_SECRET` | 随机 | 会话密钥；不设置则重启需重新登录 |
| `FIRMS_DATA` | `data/firms.json` | 费率数据文件位置 |
| `ORDERS_DB` | `data/orders.db` | 记账数据库位置（SQLite） |
| `TEMPLATES_DIR` | `templates` | 页面模板目录 |

---

## 💡 想改页面样式？

模板是独立 HTML 文件，直接改 `templates/` 下的文件，改完推 GitHub → Arcane 重建即可。

---

## ☁️ （可选）GitHub Actions 自动构建镜像

项目里附带 `.github/workflows/build.yml`（自动构建 Docker 镜像推送到 Docker Hub），**用 Arcane 的 Git 构建方式部署时不需要它**。如果以后想用「拉取镜像」方式部署（VPS 上完全没有代码），再按该文件头部的说明配置 Docker Hub 即可。
