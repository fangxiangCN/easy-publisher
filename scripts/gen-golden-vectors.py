#!/usr/bin/env python3
"""重新生成签名黄金向量 go/testdata/golden/signing.json。

# 这个脚本为什么存在

黄金向量是签名正确性的唯一护栏：Go 的实现必须对同样输入产出同样的字节。
原先它由一个 Kotlin 测试生成，但 Kotlin 版已移除（Go 版成为唯一实现），
因此把生成能力挪到这里，用 Python 的标准库独立实现一遍。

用 Python 而不是 Go 来生成，是为了保持**独立性**：如果生成器和被测实现
是同一份代码，向量就变成了自证 —— 实现错了向量跟着错，测试仍然全绿。
Python 的 hmac / hashlib 与 Go 的 crypto 是两套独立实现，对照才有意义。

# 用法

    python3 scripts/gen-golden-vectors.py            # 重新生成
    python3 scripts/gen-golden-vectors.py --check    # 只校验现有文件是否一致

改签名算法时才需要重新生成。平时这个脚本应当报「一致」——
不一致说明实现与规范脱节了。
"""

from __future__ import annotations

import hashlib
import hmac
import json
import sys
from pathlib import Path

GOLDEN_PATH = Path(__file__).resolve().parent.parent / "go" / "testdata" / "golden" / "signing.json"

FIXED_TIMESTAMP = 1700000000000

COMMENT = (
    "由 scripts/gen-golden-vectors.py 生成，供 Go 实现逐字节比对。"
    "用 Python 独立实现是为了避免自证 —— 详见该脚本的注释。"
)

# ---- 固定用例 ----
#
# 输入与原先 Kotlin 生成器逐字一致，这样向量内容不变。

OPPO_CASES = [
    {
        "name": "app-info-典型请求",
        "secret": "test-oppo-secret-0123456789",
        # api_sign 为 None：签名前尚未计算，Canonicalize 会整体跳过
        "params": {
            "access_token": "test-access-token",
            "timestamp": "1700000000000",
            "pkg_name": "com.example.app",
            "api_sign": None,
        },
    },
    {
        "name": "含特殊字符与中文",
        "secret": "test-secret-with-&-and-=-chars",
        "params": {
            "update_desc": "修复 A&B=C 的问题，支持中文",
            "pkg_name": "com.example.app",
            "timestamp": "1700000000001",
        },
    },
    {
        "name": "单参数",
        "secret": "s",
        "params": {"timestamp": "1"},
    },
]

VIVO_ACCESS_KEY = "test-vivo-access-key"

VIVO_CASES = [
    {
        "name": "提交更新",
        "secret": "test-vivo-access-secret",
        "method": "app.sync.update.app",
        "params": {
            "packageName": "com.example.app",
            "versionCode": "1020",
            "apk": "serialnumber-abc",
            "fileMd5": "d41d8cd98f00b204e9800998ecf8427e",
            "onlineType": "1",
            "updateDesc": "修复若干问题",
        },
    },
    {
        "name": "查询应用详情",
        "secret": "test-vivo-access-secret",
        "method": "app.sync.getappinfo",
        "params": {"packageName": "com.example.app"},
    },
]

MI_QUERY_CASES = [
    {"name": "查询-典型", "account": "test@example.com", "packageName": "com.example.app"},
]

MI_PUSH_CASES = [
    {
        "name": "推送-立即发布",
        "account": "test@example.com",
        "appName": "示例应用",
        "packageName": "com.example.app",
        "updateDesc": "修复若干问题",
        "onlineTime": 0,
    },
    {
        "name": "推送-定时发布",
        "account": "test@example.com",
        "appName": "示例应用",
        "packageName": "com.example.app",
        "updateDesc": "修复若干问题",
        "onlineTime": 1767225600000,
    },
    {
        "name": "推送-中文与特殊字符",
        "account": "test@example.com",
        "appName": "示例&应用",
        "packageName": "com.example.app",
        "updateDesc": '修复 A&B "引号" <尖括号>',
        "onlineTime": 0,
    },
]


# ---- 算法 ----


def oppo_canonicalize(params: dict[str, str | None]) -> str:
    """OPPO 待签串：键字典序，值为 None 的项整体跳过。"""
    return "&".join(
        f"{key}={params[key]}" for key in sorted(params) if params[key] is not None
    )


def vivo_canonicalize(params: dict[str, str]) -> str:
    """vivo 待签串：键字典序，值为空串仍参与。"""
    return "&".join(f"{key}={params[key]}" for key in sorted(params))


def hmac_sha256_hex(data: str, key: str) -> str:
    return hmac.new(key.encode(), data.encode(), hashlib.sha256).hexdigest()


def md5_hex(text: str) -> str:
    return hashlib.md5(text.encode()).hexdigest()


def vivo_signed_params(
    access_key: str, access_secret: str, method: str, params: dict[str, str]
) -> tuple[str, str, dict[str, str]]:
    """补齐公共参数并计算签名，返回 (待签串, 签名, 完整参数表)。"""
    merged = dict(params)
    # 七项公共参数由 vivo 网关规定，多一个少一个都会鉴权失败
    merged["access_key"] = access_key
    merged["timestamp"] = str(FIXED_TIMESTAMP)
    merged["method"] = method
    merged["v"] = "1.0"
    merged["sign_method"] = "HMAC-SHA256"
    merged["format"] = "json"
    merged["target_app_key"] = "developer"

    # sign 自身不参与待签串，必须在拼串之后才放进表
    canonical = vivo_canonicalize(merged)
    signature = hmac_sha256_hex(canonical, access_secret)
    merged["sign"] = signature
    return canonical, signature, merged


def mi_json(payload: dict) -> str:
    """小米的 RequestData JSON。

    必须与 Go 侧的编码逐字节一致：紧凑分隔符、不转义 HTML 字符
    （Go 默认会把 & < > 转成 \\u0026 等，Moshi 与 json.dumps 都不转义）。
    """
    return json.dumps(payload, ensure_ascii=False, separators=(",", ":"))


# ---- 构造 ----


def build() -> dict:
    oppo = []
    for case in OPPO_CASES:
        canonical = oppo_canonicalize(case["params"])
        # 输出里的 params 丢掉 None 项，与原先 Moshi 的行为一致
        visible = {k: v for k, v in case["params"].items() if v is not None}
        oppo.append(
            {
                "name": case["name"],
                "secret": case["secret"],
                "params": visible,
                "canonical": canonical,
                "signature": hmac_sha256_hex(canonical, case["secret"]),
            }
        )

    vivo = []
    for case in VIVO_CASES:
        canonical, signature, merged = vivo_signed_params(
            VIVO_ACCESS_KEY, case["secret"], case["method"], case["params"]
        )
        vivo.append(
            {
                "name": case["name"],
                "accessKey": VIVO_ACCESS_KEY,
                "accessSecret": case["secret"],
                "method": case["method"],
                "originParams": case["params"],
                "timestampMillis": FIXED_TIMESTAMP,
                "canonical": canonical,
                "signature": signature,
                "signedParams": merged,
            }
        )

    mi_query = []
    for case in MI_QUERY_CASES:
        payload = {"userName": case["account"], "packageName": case["packageName"]}
        text = mi_json(payload)
        mi_query.append({**case, "requestDataJson": text, "requestDataMd5": md5_hex(text)})

    mi_push = []
    for case in MI_PUSH_CASES:
        app_info = {
            "appName": case["appName"],
            "packageName": case["packageName"],
            "updateDesc": case["updateDesc"],
        }
        # onlineTime 为 0 时整个字段省略，与小米接口约定一致
        if case["onlineTime"] > 0:
            app_info["onlineTime"] = case["onlineTime"]
        payload = {
            "userName": case["account"],
            # 1 = 更新已有 app，发版场景固定如此
            "synchroType": 1,
            "appInfo": app_info,
        }
        text = mi_json(payload)
        mi_push.append({**case, "requestDataJson": text, "requestDataMd5": md5_hex(text)})

    return {
        "_comment": COMMENT,
        "fixedTimestampMillis": FIXED_TIMESTAMP,
        "oppo": oppo,
        "vivo": vivo,
        "miQueryRequestData": mi_query,
        "miPushRequestData": mi_push,
    }


def render(data: dict) -> str:
    """渲染成与提交文件一致的格式：2 空格缩进、中文原样、末尾无换行。"""
    return json.dumps(data, ensure_ascii=False, indent=2)


def main() -> int:
    check_only = "--check" in sys.argv
    generated = render(build())

    existing = GOLDEN_PATH.read_text() if GOLDEN_PATH.exists() else None

    if check_only:
        if existing == generated:
            print(f"一致：{GOLDEN_PATH.relative_to(GOLDEN_PATH.parents[2])}")
            return 0
        print("不一致：生成的向量与提交的文件不同。", file=sys.stderr)
        print("若确实改了签名算法，运行不带 --check 的本脚本重新生成。", file=sys.stderr)
        return 1

    GOLDEN_PATH.write_text(generated)
    action = "已更新" if existing is not None else "已创建"
    print(f"{action}：{GOLDEN_PATH}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
