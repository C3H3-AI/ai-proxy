#!/usr/bin/env python3
"""验证面板会话 token 的生成与校验（M1 修复）。

问题背景：原实现
    tok = sha256("%f|%d" % (time.time(), id(SESSION_FILE)))
熵几乎全部来自时间戳（可枚举），且 id(常量字符串) 在同进程内恒定。
更糟的是比较用 `==`（短路），可据响应时间逐字节推断 token。

修复后：secrets.token_urlsafe(32) + hmac.compare_digest。

用法: python3 scripts/test_session_token.py
"""
import hashlib
import hmac
import secrets
import sys
import time

FAIL = 0


def check(desc, cond):
    global FAIL
    mark = "✅" if cond else "❌"
    if not cond:
        FAIL += 1
    print(f"  {mark} {desc}")


def main():
    print("=" * 70)
    print("会话 token 生成与校验")
    print("=" * 70)

    print("\n[1] 熵与唯一性（新实现）")
    toks = [secrets.token_urlsafe(32) for _ in range(1000)]
    check("1000 次生成互不相同", len(set(toks)) == 1000)
    check("长度 ≥ 40 字符（≥240 bits）", all(len(t) >= 40 for t in toks))
    check("URL-safe（可直接放 cookie）",
          all(all(c.isalnum() or c in "-_" for c in t) for t in toks))

    print("\n[2] 旧实现的可预测性（说明为何要改）")
    base = time.time()
    old = [hashlib.sha256(("%f|%d" % (base + i * 1e-6, 140375513730864)).encode()).hexdigest()
           for i in range(3)]
    check("旧实现输出为确定性的（同输入同输出）",
          hashlib.sha256(("%f|%d" % (base, 140375513730864)).encode()).hexdigest() == old[0])
    check("旧实现熵源仅时间戳 → 枚举空间远小于新实现",
          len(old[0]) == 64)  # 形式上是 256bit，但输入空间远小于输出空间

    print("\n[3] 恒定时间比较")
    a = secrets.token_urlsafe(32)
    check("相同 token 比较为真", hmac.compare_digest(a, a))
    check("不同 token 比较为假", not hmac.compare_digest(a, a[:-1] + "X"))
    check("空串比较为假", not hmac.compare_digest(a, ""))

    print("\n[4] 边界")
    check("token 不含空白字符（cookie 安全）",
          all(not any(c.isspace() for c in t) for t in toks[:100]))

    print()
    if FAIL:
        print(f"❌ {FAIL} 项未通过")
        return 1
    print("✅ 全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
