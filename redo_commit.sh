#!/bin/sh
# 彻底重做提交：unstage 全部后依赖 .gitignore 重新 add
cd /d/ai-hub/integrations/ha-ai-proxy || exit 1
git reset --soft bde083c
git reset -q
printf '.mimosa/\ntrae-tokens-local/\ndeploy_commit.sh\ncommit_restore.sh\nrebuild_commit.sh\n' > .gitignore
git add -A
echo "=== 提交前检查（应无 .mimosa / trae-tokens-local）==="
git diff --cached --name-only | grep -E 'mimosa|trae-tokens|commit_restore|rebuild_commit' && { echo "仍有垃圾文件，中止"; exit 1; }
git -c user.name="C3H3-AI" -c user.email="deploy@homediy.top" commit -q -m "feat: 恢复 b3-b12 全部改动 — 设置统一WebUI/安全加固/禁用启用/cheapest改名/虚拟模型/自动保存白名单 (1.1.0b13)"
git push --force origin master 2>&1 | tail -1
echo "=== 最终提交文件数与抽查 ==="
git show --stat HEAD | tail -3
git show --name-only HEAD | grep -cE 'mimosa|trae-tokens' || echo "0 个垃圾文件 ✓"
