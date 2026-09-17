# 📓 项目笔记（对话保存）—— 用车账单系统 v2.0.0

> 用途：下次继续开发时，把这份文件给豆包看，即可无缝续接，不用重新解释背景。
> 最后更新：2026-09-17

---

## 一、项目是什么

一个「用车账单生成器」网页小工具，司机/车主在微信里快速生成对外报价账单，并带有**订单记账后台**。

- **技术栈**：Go（标准库 net/http）+ SQLite（modernc.org/sqlite，纯 Go 无 CGO）+ Docker
- **形态**：单 Docker 容器，网页访问，后台 /admin
- **GitHub 仓库**：`https://github.com/luobingwei/car-billing-go`（v2.0.0）
- **本机代码目录**：`/home/user/Doubao/chats/38421063805886978/car-billing/`
- **交付包**：`car-billing-go-v2.zip`（在代码目录上级，实为 v2.0.0 内容）

## 二、功能清单（v2.0.0 全部已实现并验证）

### 前台（/）
- 用车类型：**顶部标签页**（接送机 / 包车 / 点对点），手机友好
- 行程路线输入：地址之间用**空格**分隔，生成时自动转 **→**（例：梦工厂 → 梦工厂A区 → 宝马家居）
- 对接人自动加 **@** 前缀
- 「生成账单」输出结果文本，含**总金额**（从文本解析）
- 「**一键复制账单**」→ 系统剪贴板（textarea + execCommand('copy') 为主，navigator.clipboard 兜底，iPhone Safari 可用）
- 「**保存到记账本**」→ POST /api/order/save（前台免登录）
- 实收金额输入框：默认等于总金额，可手动改

### 后台（/admin，密码 ADMIN_PASSWORD 默认 123456）
- 双 Tab：**费率配置**（原功能原样保留）+ **记账管理**
- 记账管理：统计卡（全部/未收/已收）、筛选（公司/收款状态/日期范围）、表格或手机卡片列表、账单内容展开+复制、**收钱开关（confirm 二次确认）**、删除
- 导出 **xlsx / csv**（带当前筛选条件），WPS/Excel 可打开
- 手机/电脑自适应

### 数据库（data/orders.db，SQLite）
- 表 orders 字段：ID、Company、Date、Amount(总金额)、Received(实收)、Contact、Content(结果文本)、Paid、CreatedAt
- 表 history 字段：ID、Kind(contact/customer/flight/route)、Value、CreatedAt（前台输入历史，每类最多 10 条，存数据库非 localStorage）
- 表 firms 字段：ID、Name、Data(费率JSON)（v2.0.0 起费率也入库；firms.json 仅首次启动迁移导入，之后以数据库为准）
- 启动时 `ensureColumn` 自动补列迁移旧库，旧数据不丢
- 金额来源：保存时优先用 `parseAmountFromText` 从结果文本解析（正则 `/(?:总计|费用合计)[：:]\s*([\d.]+)\s*元/g` 取最后一个），解析不到才用输入框值

### 数据备份 / 恢复（后台）
- 「⬇ 下载数据库备份」：VACUUM INTO 生成一致性快照，文件名 `orders_2026-09-17_150405.db` 带日期时间
- 「⬆ 恢复备份（上传 .db）」：校验 SQLite 魔数（拒绝非库文件）→ 覆盖恢复 → 自动建表/补列 → 失败自动回滚（旧库 .bak）
- **备份一个 .db 文件 = 整套系统（订单 + 历史记录 + 费率）**

### 数据文件
- `data/firms.json`：费率迁移源（首次启动自动导入 SQLite，之后不再读写，可保留不动）
- `data/orders.db`：订单 + 历史 + 费率（首次运行自动生成）

## 三、关键路由

| 路由 | 说明 |
|---|---|
| `/` | 前台 |
| `/admin` | 后台（需登录，Cookie: admin_session，HMAC-SHA256，TTL 12h） |
| `/logout` | 退出 |
| `/healthz` | 健康检查 |
| `/api/order/save` | 保存订单（免登录） |
| `/api/order/list` | 列表+筛选（需登录） |
| `/api/order/toggle` | 收钱状态切换（需登录） |
| `/api/order/delete` | 删除（需登录） |
| `/api/order/export` | 导出 xlsx/csv（需登录） |
| `/api/history/save` | 保存历史（免登录，前台） |
| `/api/history/list` | 读历史，每类10条（免登录，前台） |
| `/api/backup/download` | 下载数据库备份 .db（需登录） |
| `/api/backup/restore` | 上传 .db 覆盖恢复（需登录，失败回滚） |

## 四、部署方式（用户实际用法）

- **Arcane Docker 面板 + Compose**，compose 用 **build: git URL** 语法：
  `build: https://github.com/luobingwei/car-billing-go.git`（部署时自动拉 GitHub 代码构建，更新代码后面板点重建即可）
- 端口 3333 → 8080，命名数据卷 `car-billing-data:/app/data`
- 完整 compose 见 README.md「Arcane 面板版」章节
- 版本流程：v1.0.0 是首版；以后升级 push 代码 → Arcane 重建；想新旧共存就新建项目+端口3334+独立数据卷测试，通过再切换

## 五、用户偏好（对话中确认过，开发时遵守）

1. **不喜欢多余的弹窗/提示**（原话：「不用弹出已全选的的消息，我会在手机看到全选的文本框。多此一举」）——toast 只用在必要处，轻提示
2. 结果文本**会被手动修改**（如两台车合并写一个账单），所以**金额必须从结果文本解析**，不能只信生成时算的
3. 手机是主战场（iPhone Safari），所有交互要手机友好：大按钮、顶部标签页、一键复制
4. 对部署要求：**更新测试好之后，VPS 面板里直接重建容器就能用**，不想折腾 SSH/镜像仓库

## 六、本地验证记录（可信）

- go build + go vet 通过（Go 1.23）
- curl 全流程：保存订单→未登录401→登录→列表/筛选→toggle→delete→export 全部通过
- 导出的 xlsx 用 openpyxl 打开验证表头与数据正确
- parseAmountFromText 5 个场景（正常/费用合计/两台车合并取最后总计/手动改/无金额）全通过
- 旧库迁移实测：无 amount/received 列的旧库启动新版自动补列、旧数据保留
- 历史记录实测：保存/读取/同值去重/12条只留最新10条 全通过
- 备份实测：未登录401、下载文件名带日期、快照可被 python sqlite3 正常打开、非SQLite文件上传被拒、有效备份恢复成功且服务继续正常

## 七、遗留 / 可选事项（下次可做）

- [ ] GitHub Actions（`.github/workflows/build.yml` 已备好）可配 Docker Hub 自动构建镜像（当前部署方式不需要，可选）
- [ ] 域名 + HTTPS（用户用 Nginx Proxy Manager 或 Arcane 反代，README 有提示）
- [ ] 备份自动化（firms.json / orders.db 定期备份脚本）
- [ ] 用户提到的后续功能需求（如有新需求在此追加）

## 八、会话要点速览（背景记忆）

- 早期是 PHP 版，用户明确否定后改为 **Go + SQLite + Docker** 重写
- 曾迭代：顶部标签页、空格转箭头、@对接人、一键全选→一键复制、服务日期占位修正
- 本会话完成：后台记账系统全量（保存/列表/筛选/收钱/删除/导出）、总金额+实收、GitHub 版本流程、Arcane 部署
- 用户当前动作：把「带备份+历史记录10条」版作为 v2.0.0，v1.0.0 已在 GitHub 上线，清空 GitHub 旧仓库重新上传
