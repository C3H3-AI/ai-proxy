#!/bin/sh
# 清理 HA recorder 中孤立的长期统计数据（Spook 报告的那些）
#
# 背景：
#   recorder.purge_entities 服务只清「数据」（statistics / statistics_short_term 行），
#   但保留 statistics_meta 行；Spook 检测的正是 statistics_meta 里「无对应实体」的条目，
#   所以服务调用后 Spook 警告仍在。彻底清除必须直接删 statistics_meta 行。
#
# 用法（在 HAOS 上以 root 执行）：
#   sh /mnt/data/supervisor/homeassistant/cleanup_orphan_stats.sh
#
# 建议：先在 HA 界面 设置→系统→备份 做一个备份，或至少复制 DB 文件（需 ~6G 空间）。

set -e

DB=/mnt/data/supervisor/homeassistant/home-assistant_v2.db

echo "=== 清理前 ==="
sqlite3 "$DB" "SELECT COUNT(*) FROM statistics_meta sm LEFT JOIN states_meta st ON sm.statistic_id = st.entity_id WHERE st.entity_id IS NULL;" | sed 's/^/孤立统计条目: /'

echo "=== 待删列表 ==="
sqlite3 "$DB" "SELECT sm.id, sm.statistic_id FROM statistics_meta sm LEFT JOIN states_meta st ON sm.statistic_id = st.entity_id WHERE st.entity_id IS NULL;"

# 注意：HA 运行中直接改 DB 有风险。稳妥做法是先停 core：
#   ha core stop
# 删完再 ha core start。
# 若不想停 core，SQLite 的写锁通常也能完成这小事务，但请自行评估。

echo "=== 执行删除 ==="
sqlite3 "$DB" <<'SQL'
BEGIN IMMEDIATE;

CREATE TEMP TABLE orphan_meta AS
  SELECT sm.id FROM statistics_meta sm
  LEFT JOIN states_meta st ON sm.statistic_id = st.entity_id
  WHERE st.entity_id IS NULL;

DELETE FROM statistics            WHERE metadata_id IN (SELECT id FROM orphan_meta);
DELETE FROM statistics_short_term WHERE metadata_id IN (SELECT id FROM orphan_meta);
DELETE FROM statistics_meta       WHERE id          IN (SELECT id FROM orphan_meta);

COMMIT;
SQL

echo "=== 清理后 ==="
sqlite3 "$DB" "SELECT COUNT(*) FROM statistics_meta sm LEFT JOIN states_meta st ON sm.statistic_id = st.entity_id WHERE st.entity_id IS NULL;" | sed 's/^/剩余孤立统计条目: /'

echo "=== 回收空间（可选，较慢）==="
echo "如需回收文件空间，执行: sqlite3 $DB 'VACUUM;'"
echo "完成。请到 HA 界面 设置→系统→修复 确认 Spook 警告消失；若未消失则重启 HA core。"
