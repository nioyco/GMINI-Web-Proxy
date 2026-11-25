#!/usr/bin/env python3
"""Backfill idempotent de ~/Library/Logs/gmini/gemini_tools_db.jsonl depuis proxy.log.

Lit uniquement des métadonnées techniques anonymisées (aucun secret, cookie,
clé API, ni contenu de prompt/réponse). Permissions 0600 garanties.
"""

import argparse
import json
import os
import re
import shlex
import sys
from collections import defaultdict

DEFAULT_LOG = os.path.expanduser("~/Library/Logs/gmini/proxy.log")
DEFAULT_DB = os.path.expanduser("~/Library/Logs/gmini/gemini_tools_db.jsonl")
TAG_RE = re.compile(r"-(k\d+)$", re.IGNORECASE)
KV_RE = re.compile(r'^([A-Za-z0-9_.-]+)=(.*)$')


def parse_slog_line(line):
    """Parse a slog text line into a dict of metadata fields."""
    out = {}
    try:
        tokens = shlex.split(line)
    except ValueError:
        tokens = line.split()
    for tok in tokens:
        m = KV_RE.match(tok)
        if m:
            out[m.group(1)] = m.group(2)
    return out


def extract_tag(binary_name):
    m = TAG_RE.search((binary_name or "").strip())
    return m.group(1).upper() if m else ""


def to_rfc3339(ts_raw):
    if not ts_raw:
        return ""
    return ts_raw


def backfill(log_path, db_path):
    existing_keys = set()
    existing_lines = []
    if os.path.isfile(db_path):
        with open(db_path, "r", encoding="utf-8", errors="replace") as f:
            for raw in f:
                s = raw.strip()
                if not s:
                    continue
                existing_lines.append(s)
                try:
                    rec = json.loads(s)
                    existing_keys.add((rec.get("request_id", ""), rec.get("tool_kind", ""), rec.get("ts", "")))
                except json.JSONDecodeError:
                    pass

    if not os.path.isfile(log_path):
        return 0, len(existing_lines)

    reqs = defaultdict(lambda: {
        "ts": "",
        "model": "",
        "served_model": "",
        "prompt_bytes": 0,
        "duration_ms": 0,
        "ttft_ms": 0,
        "resp_bytes": 0,
        "attempts": 1,
        "status": "",
        "internal_tools": [],
        "tool_refusal": False,
        "glitch": False,
        "stall": False,
        "tool_calls": 0,
        "binary_version": "",
        "binary_tag": "",
    })

    cur_bin = ""
    cur_tag = ""
    with open(log_path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            kv = parse_slog_line(line)
            if not kv:
                continue
            msg = kv.get("msg", "")
            if msg == "proxy_listening" or kv.get("binary_name"):
                bn = kv.get("binary_name", "")
                if bn:
                    cur_bin = bn
                    cur_tag = extract_tag(bn)
            rid = kv.get("request_id", "")
            if not rid:
                continue
            r = reqs[rid]
            if kv.get("time") and not r["ts"]:
                r["ts"] = to_rfc3339(kv["time"])
            if cur_bin and not r["binary_version"]:
                r["binary_version"] = cur_bin
                r["binary_tag"] = cur_tag
            if kv.get("model") and not r["model"]:
                r["model"] = kv["model"]
            if kv.get("served_model"):
                r["served_model"] = kv["served_model"]
            if msg == "request_end":
                r["ts"] = to_rfc3339(kv.get("time", r["ts"]))
                r["status"] = kv.get("status", r["status"])
                for k_src, k_dst in (
                    ("prompt_bytes", "prompt_bytes"),
                    ("duration_ms", "duration_ms"),
                    ("first_byte_ms", "ttft_ms"),
                    ("resp_bytes", "resp_bytes"),
                    ("upstream_attempts", "attempts"),
                ):
                    if kv.get(k_src, "").isdigit():
                        r[k_dst] = int(kv[k_src])
            elif msg == "internal_tool_detected":
                tname = kv.get("tool", "")
                if tname and tname not in r["internal_tools"]:
                    r["internal_tools"].append(tname)
            elif msg == "upstream_tool_refusal" and kv.get("action") == "reported":
                r["tool_refusal"] = True
            elif msg == "upstream_glitch" and kv.get("action") == "reported":
                r["glitch"] = True
            elif msg == "early_stop" and kv.get("reason") == "stall":
                r["stall"] = True
            elif msg in ("tool_calls_emitted", "code_ref_fastpath"):
                r["tool_calls"] = max(r["tool_calls"], 1)

    added = []
    for rid, r in reqs.items():
        if not r["model"] or not r["ts"]:
            continue
        has_internal = len(r["internal_tools"]) > 0
        has_local_fail = r["tool_refusal"] or r["glitch"] or r["stall"]
        has_local_ok = r["tool_calls"] > 0
        if not has_internal and not has_local_fail and not has_local_ok:
            continue

        base = {
            "ts": r["ts"],
            "binary_version": r["binary_version"],
            "binary_tag": r["binary_tag"],
            "request_id": rid,
            "model_requested": r["model"],
            "served_model": r["served_model"],
            "prompt_bytes": r["prompt_bytes"],
            "duration_ms": r["duration_ms"],
            "ttft_ms": r["ttft_ms"],
            "response_bytes": r["resp_bytes"],
            "attempts": max(1, r["attempts"]),
        }

        if has_internal:
            key = (rid, "internal_google", r["ts"])
            if key not in existing_keys:
                existing_keys.add(key)
                rec = dict(base)
                rec["tool_kind"] = "internal_google"
                rec["tool_names"] = r["internal_tools"]
                rec["tool_count"] = len(r["internal_tools"])
                if r["stall"]:
                    rec["proxy_action"] = "early_stop"
                    rec["outcome"] = "stall"
                    rec["note"] = "backfill: outil interne + stall"
                elif r["glitch"]:
                    rec["proxy_action"] = "reported"
                    rec["outcome"] = "glitch"
                    rec["note"] = "backfill: outil interne + glitch"
                elif r["tool_refusal"]:
                    rec["proxy_action"] = "reported"
                    rec["outcome"] = "tool_refusal"
                    rec["note"] = "backfill: outil interne + refus"
                else:
                    rec["proxy_action"] = "retry" if r["attempts"] > 1 else "reported"
                    rec["outcome"] = "recovered"
                    rec["note"] = "backfill: outil interne détecté"
                added.append(rec)

        if has_local_ok or (has_local_fail and not has_internal):
            key = (rid, "local", r["ts"])
            if key not in existing_keys:
                existing_keys.add(key)
                rec = dict(base)
                rec["tool_kind"] = "local"
                rec["tool_names"] = []
                rec["tool_count"] = 0
                if has_local_ok and not has_local_fail:
                    rec["proxy_action"] = "retry" if r["attempts"] > 1 else "forwarded"
                    rec["outcome"] = "recovered" if r["attempts"] > 1 else "tool_calls_ok"
                elif r["stall"]:
                    rec["proxy_action"] = "early_stop"
                    rec["outcome"] = "stall"
                    rec["note"] = "backfill: stall outil"
                elif r["glitch"]:
                    rec["proxy_action"] = "reported"
                    rec["outcome"] = "glitch"
                    rec["note"] = "backfill: glitch"
                else:
                    rec["proxy_action"] = "reported"
                    rec["outcome"] = "tool_refusal"
                    rec["note"] = "backfill: refus outils"
                added.append(rec)

    if added:
        os.makedirs(os.path.dirname(db_path), mode=0o700, exist_ok=True)
        with open(db_path, "a", encoding="utf-8") as f:
            for rec in added:
                f.write(json.dumps(rec, ensure_ascii=False) + "\n")
        os.chmod(db_path, 0o600)

    return len(added), len(existing_lines) + len(added)


def main():
    p = argparse.ArgumentParser(description="Backfill anonymisé de gemini_tools_db.jsonl")
    p.add_argument("--log", default=DEFAULT_LOG)
    p.add_argument("--db", default=DEFAULT_DB)
    args = p.parse_args()
    added, total = backfill(args.log, args.db)
    print(f"backfill OK: +{added} entrée(s) ajoutée(s), total={total} ({args.db})")


if __name__ == "__main__":
    main()
