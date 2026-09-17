#!/bin/bash
# ============================================
# 用车账单生成器 - VPS 一键部署脚本
# 在 VPS 上执行: bash deploy.sh
# ============================================
set -e

echo ">>> 第 1 步：检查 Docker..."

# 1. 检查并安装 Docker
if ! command -v docker >/dev/null 2>&1; then
  echo "未检测到 Docker，开始自动安装..."
  curl -fsSL https://get.docker.com | sh
  systemctl enable --now docker
  echo "Docker 安装完成"
else
  echo "Docker 已安装: $(docker --version)"
fi

# 2. 如果项目文件夹不存在，从 GitHub 克隆（改成你自己的仓库地址）
if [ ! -d "car-billing-go" ]; then
  echo ">>> 第 2 步：从 GitHub 克隆代码..."
  git clone https://github.com/你的用户名/car-billing-go.git
fi
cd car-billing-go

# 3. 构建并启动
echo ">>> 第 3 步：构建并启动容器..."
if docker compose version >/dev/null 2>&1; then
  docker compose up -d --build
else
  docker-compose up -d --build
fi

# 4. 完成提示
IP=$(curl -s -4 ifconfig.me 2>/dev/null || echo "你的服务器IP")
echo ""
echo "=============================================="
echo "✅ 部署完成！"
echo "  首页: http://$IP:3333"
echo "  后台: http://$IP:3333/admin   (密码: 123456)"
echo "=============================================="
echo "常用命令: docker logs -f car-billing  (看日志)"
