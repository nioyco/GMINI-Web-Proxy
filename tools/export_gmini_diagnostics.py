#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
export_gmini_diagnostics.py — Export anonymisé des diagnostics du proxy GMINI.

Parse `proxy.log` (slog) et `traces/*.json` (traces par requête) et produit,
par défaut ANONYMISÉ (aucun cookie/clé/contenu de prompt/fichier/réponse ;
uniquement tailles, hashes, types d'événements et noms d'outils) :

  * un CSV/JSON par requête (métriques Phase 1) ;
  * un résumé agrégé (taux de succès par modèle/taille/outil/binaire, TTFT,
    retries, familles d'incidents) ;
  * la liste des incidents regroupés par cause avec req_id représentatifs ;
  * un rapport Markdown.

Contraintes :
  * Python 3 standard library uniquement (aucune dépendance réseau) ;
  * idempotent (relance = même résultat) ;
  * plante proprement si un motif de secret est détecté dans la sortie.

Formats (--format) :
  * liste séparée par des virgules, sous-ensemble de json,csv,md
    (ex. `--format json,csv`), ou le jeton spécial `all` (= json,csv,md) ;
  * défaut = tous les formats ;
  * json -> requests.json + summary.json + incidents.json ;
    csv -> requests.csv ; md -> report.md ;
  * excluded.txt est toujours écrit (il documente l'anonymisation) ;
  * excerpts.md est écrit dès que --include-excerpts N est > 0,
    quel que soit --format (option opt-in, contenu non sensible uniquement).

Issue finale (par requête) :
  tool_calls_ok | text_only_legit | text_only_suspect |
  no_content_timeout | tool_refusal | canned_error | glitch | client_cancelled |
  stall_exhausted | initial_silence_exhausted | empty | error
  Le taux de succès ne compte que tool_calls_ok + text_only_legit
  (jamais text_only_suspect).

Usage :
  python3 tools/export_gmini_diagnostics.py [--since S] [--until U] [--last D]
      [--binary NAME] [--model NAME] [--req-id ID] [--out DIR]
      [--format json,csv,md|all] [--include-excerpts N] [--selftest] [--help]
"""

import argparse
import collections
import csv
import datetime as dt
import glob
import hashlib
import io
import json
import os
import re
import sys
import tempfile

DEFAULT_LOG = os.path.expanduser("~/Library/Logs/gmini/proxy.log")
DEFAULT_TRACES = os.path.expanduser("~/Library/Logs/gmini/traces")

VERSION = "1.2.0"

# Formats de sortie connus, dans l'ordre canonique de production.
FORMATS = ("json", "csv", "md")

#: Jeton spécial : tous les formats.
FORMAT_ALL = "all"


def parse_formats(value):
    """Normalise une valeur de --format en liste ordonnée de formats.

    Accepte une liste séparée par des virgules (`json,csv`), un sous-ensemble
    quelconque (`md`), ou le jeton `all` (= json,csv,md). None => tous les
    formats (comportement par défaut historique). Lève SystemExit avec un
    message explicite si un jeton est inconnu.
    """
    if value is None:
        return list(FORMATS)
    if isinstance(value, (list, tuple)):
        tokens = [str(v) for v in value]
    else:
        tokens = str(value).split(",")
    chosen = []
    for tok in tokens:
        t = tok.strip().lower()
        if not t:
            continue
        if t == FORMAT_ALL:
            for f in FORMATS:
                if f not in chosen:
                    chosen.append(f)
            continue
        if t not in FORMATS:
            raise SystemExit(
                f"--format inconnu : {tok.strip()!r} "
                f"(attendu : {','.join(FORMATS)} ou {FORMAT_ALL}, "
                f"ou une liste comme json,csv)")
        if t not in chosen:
            chosen.append(t)
    if not chosen:
        raise SystemExit(
            f"--format vide (attendu : {','.join(FORMATS)} ou {FORMAT_ALL})")
    return [f for f in FORMATS if f in chosen]

# Motifs de secret : s'ils apparaissent dans la SORTIE, on plante avant écriture.
SECRET_PATTERNS = [
    re.compile(r"AIza[0-9A-Za-z_\-]{20,}"),
    re.compile(r"\bsk-[A-Za-z0-9_\-]{20,}"),
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    re.compile(r"\bBearer\s+[A-Za-z0-9._\-]{20,}", re.I),
    re.compile(r"\beyJ[A-Za-z0-9_\-]{20,}\.[A-Za-z0-9_\-]{10,}"),
    re.compile(r"(?:^|[^A-Za-z0-9])(?:api[_-]?key|apikey|access[_-]?token|auth[_-]?token)\s*[:=]\s*\S{8,}", re.I),
    re.compile(r"(?:SID|HSID|SSID|APISID|SAPISID|__Secure-[0-9A-Za-z]+|NID|LSID|CONSENT|AEC|OTZ)\s*[:=]\s*\S{8,}"),
    re.compile(r"\bsession[_-]?token\s*[:=]\s*\S{12,}", re.I),
]

PROMPT_BUCKETS = [
    ("<50K", 50 * 1024),
    ("50-150K", 150 * 1024),
    ("150-300K", 300 * 1024),
    ("300-450K", 450 * 1024),
    ("450-600K", 600 * 1024),
    (">600K", None),
]

TOOL_CALL_FENCE = re.compile(r"```\s*(tool_call|function_call)\b", re.I)
TOOL_NAME_RE = re.compile(r'"name"\s*:\s*"([A-Za-z_][A-Za-z0-9_]{0,40})"')

KNOWN_TOOLS = (
    "read_file", "write_file", "edit_file", "run_command", "list_files",
    "search_files", "grep", "glob", "ask_question", "todo", "finish",
    "search_web", "bash", "execute_command",
)

# Message "service" = court + motif connu => non sensible (canned/refus/glitch).
_SERVICE_RE = re.compile(
    r"i['’]m having a hard time|can['’]t use workspace|turn on these settings|"
    r"permission\s+to\s+turn\s+on|autorisation\s+d['’]activer|"
    r"quota|appels?\s+d['’]outils|tool\s+calls?\s+remaining|envoie\s+.*continue|"
    r"GMINI\s*:|seem to be encountering an error|renvoyé une erreur interne", re.I)

# Refus / handoff (miroir des détecteurs Go : apiconv.IsUpstreamToolRefusal,
# IsPostStallHandoffSummary). Utilisés UNIQUEMENT comme booléens de
# classification : aucun texte de réponse n'est stocké ni exporté.
_REFUSAL_RE = re.compile(
    r"i['’]m having a hard time|hard time fulfilling|can['’]t help with that|"
    r"can['’]t use workspace|permission\s+to\s+turn\s+on|turn on these settings|"
    r"tools?\s+are\s+(?:not\s+available|unavailable)|"
    r"no\s+(?:local\s+|file\s+|command\s+)?tools\s+(?:are\s+)?(?:available|declared)|"
    r"without\s+(?:any\s+|access\s+to\s+|using\s+|calling\s+)?(?:file\s+|local\s+)?tools?|"
    r"tool\s+calls?\s+remaining|appels?\s+d['’]outils|envoie\s+.*continue|"
    r"renvoyé\s+une\s+erreur\s+interne", re.I)

# « bilan d'étapes » / handoff post-stall : liste d'avancement au lieu d'un
# appel d'outil (cf. postStallHandoffRe côté Go).
_HANDOFF_RE = re.compile(
    r"état(?:\s+(?:exact|actuel))?\s+d['’]avancement|étapes\s+restantes|"
    r"fichiers\s+(?:et\s+étapes\s+)?restant(?:s|es)?|"
    r"dernières\s+actions\s+à\s+exécuter|prochaines\s+étapes|"
    r"remaining\s+steps|files\s+remaining|bilan\s+d['’]étapes", re.I)


# ---------------------------------------------------------------------------
# Parsing slog
# ---------------------------------------------------------------------------

_SLOG_KV = re.compile(r'([A-Za-z0-9_.]+)=("(?:[^"\\]|\\.)*"|\'[^\']*\'|[^\s]*)')


def _unquote(v):
    if len(v) >= 2 and v[0] in "\"'" and v[-1] == v[0]:
        body = v[1:-1]
        if v[0] == '"':
            body = body.replace('\\"', '"').replace("\\\\", "\\")
        return body
    return v


def parse_slog_line(line):
    m = re.match(r"time=(\S+) level=(\S+) msg=(\"[^\"]*\"|\S+)", line)
    if not m:
        return None
    ts, level, msg = m.group(1), m.group(2), _unquote(m.group(3))
    attrs = {}
    for k, v in _SLOG_KV.findall(line):
        attrs[k] = _unquote(v)
    return ts, level, msg, attrs


def parse_iso_ts(s):
    if not s:
        return None
    s = s.strip().replace("Z", "+00:00")
    d = None
    try:
        d = dt.datetime.fromisoformat(s)
    except ValueError:
        for fmt in ("%Y-%m-%dT%H:%M:%S.%f%z", "%Y-%m-%dT%H:%M:%S%z",
                    "%Y-%m-%d %H:%M:%S", "%Y-%m-%dT%H:%M:%S"):
            try:
                d = dt.datetime.strptime(s, fmt)
                break
            except ValueError:
                continue
    if d is None:
        return None
    if d.tzinfo is not None:
        d = d.replace(tzinfo=None)  # wall-clock local (tous les logs partagent la même zone)
    return d


def parse_user_dt(s, end_of_day=False):
    if s is None:
        return None
    s = s.strip()
    now = dt.datetime.now().astimezone()
    tz = now.tzinfo
    out = []
    for fmt in ("%Y-%m-%dT%H:%M:%S", "%Y-%m-%d %H:%M:%S", "%Y-%m-%d"):
        try:
            d = dt.datetime.strptime(s, fmt)
            if "%H" not in fmt and end_of_day:
                d = d.replace(hour=23, minute=59, second=59)
            out.append(d.replace(tzinfo=tz))
        except ValueError:
            pass
    m = re.match(r"^(\d{1,2}):(\d{2})$", s)
    if m:
        out.append(now.replace(hour=int(m.group(1)), minute=int(m.group(2)), second=0, microsecond=0))
    if not out:
        raise SystemExit(f"Date non reconnue : {s!r} (attendu AAAA-MM-JJ[THH:MM:SS] ou HH:MM)")
    d = out[0]
    if d.tzinfo is not None:
        d = d.replace(tzinfo=None)
    return d


_DURATION_RE = re.compile(r"^(\d+)([smhd])$", re.I)


def parse_duration(s):
    """Parse une durée relative pour --last (ex. 15m, 1h, 6h, 24h, 7d)."""
    if s is None:
        return None
    raw = str(s).strip()
    m = _DURATION_RE.match(raw)
    if not m:
        raise SystemExit(
            f"Durée --last non reconnue : {s!r} "
            "(attendu <N><s|m|h|d>, ex. 15m, 1h, 6h, 24h, 7d)")
    val = int(m.group(1))
    unit = m.group(2).lower()
    if val <= 0:
        raise SystemExit(f"Durée --last invalide (doit être > 0) : {s!r}")
    if unit == "s":
        return dt.timedelta(seconds=val)
    if unit == "m":
        return dt.timedelta(minutes=val)
    if unit == "h":
        return dt.timedelta(hours=val)
    return dt.timedelta(days=val)


# ---------------------------------------------------------------------------
# Marqueurs de démarrage binaire
# ---------------------------------------------------------------------------

_STARTUP_TS = re.compile(r"(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})")
_STARTUP_BIN = re.compile(r"binaire=(\S+)")
_STARTUP_NAME = re.compile(r"(?:binaire\s+)?(P\d+|go-gemini-web2api[\w.\-]+)")


def parse_binary_timeline(lines):
    markers = []
    for line in lines:
        if "démarrage" not in line and "redémarrage" not in line:
            continue
        m = _STARTUP_TS.search(line)
        if not m:
            continue
        ts = parse_iso_ts(m.group(1).replace(" ", "T"))
        if ts is None:
            continue
        b = _STARTUP_BIN.search(line)
        name = b.group(1) if b else ""
        if not name:
            n = _STARTUP_NAME.search(line)
            name = n.group(1) if n else "unknown"
        markers.append((ts, name))
    return sorted(markers, key=lambda x: x[0])


def binary_at(markers, ts):
    if ts is None:
        return "unknown"
    name = None
    for mts, mname in markers:
        if mts <= ts:
            name = mname
        else:
            break
    return name or "unknown"


# ---------------------------------------------------------------------------
# Lecture des traces (metadata uniquement)
# ---------------------------------------------------------------------------

def hash_short(s, min_len=64, n=16):
    if s is None or len(s) < min_len:
        return ""
    return "h:" + hashlib.sha256(s.encode("utf-8", "replace")).hexdigest()[:n]


def _is_service_message(text):
    return 0 < len(text) <= 500 and bool(_SERVICE_RE.search(text))


def _extract_wrb_text(inner, out):
    """Extrait le texte des candidats d'un corps wrb.fr (inner[4][*][1][*])."""
    if not isinstance(inner, list):
        return
    try:
        cands = inner[4]
    except (IndexError, TypeError):
        cands = None
    if isinstance(cands, list):
        for c in cands:
            if isinstance(c, list) and len(c) > 1 and isinstance(c[1], list):
                for s in c[1]:
                    if isinstance(s, str):
                        out.append(s)
        return
    for it in inner:
        _extract_wrb_text(it, out)


def decode_raw_text(raw_frames):
    """Reconstruit le texte (anonymisé : sert uniquement au comptage) des frames wrb.fr."""
    parts = []
    for fr in raw_frames:
        if not isinstance(fr, str):
            continue
        cur = fr
        decoded_any = False
        for _ in range(3):  # jusqu'à 3 niveaux d'encodage JSON chaîné
            try:
                o = json.loads(cur)
                decoded_any = True
            except Exception:
                break
            if isinstance(o, str):
                cur = o
                continue
            if isinstance(o, list):
                for item in o:
                    if isinstance(item, list) and len(item) >= 3 and item[0] == "wrb.fr":
                        s = item[2]
                        for __ in range(3):
                            try:
                                inner = json.loads(s)
                                if isinstance(inner, str):
                                    s = inner
                                    continue
                                _extract_wrb_text(inner, parts)
                                break
                            except Exception:
                                break
                    elif isinstance(item, list):
                        _extract_wrb_text(item, parts)
                break
        if not decoded_any and ("```" in fr or "tool_call" in fr.lower()):
            parts.append(fr)  # texte brut (jeux de logs synthétiques)
    return "".join(parts)


def scan_tool_calls(raw_frames):
    # Les backticks ne sont pas échappés en JSON : compter les fences dans les
    # frames brutes (robuste) ET dans le texte décodé (pour les noms).
    raw_blocks = 0
    for fr in raw_frames:
        if isinstance(fr, str):
            raw_blocks += len(TOOL_CALL_FENCE.findall(fr))
    text = decode_raw_text(raw_frames)
    dec_blocks = len(TOOL_CALL_FENCE.findall(text))
    blocks = max(raw_blocks, dec_blocks)
    names = sorted({nm for nm in TOOL_NAME_RE.findall(text) if nm in KNOWN_TOOLS})
    return blocks, names


def load_trace(path):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            d = json.load(f)
    except Exception as e:
        return {"_err": f"{os.path.basename(path)}: {e}"}
    out = {}
    for k in ("time", "request_id", "model", "model_id", "thinking_level", "mode",
              "stream", "duration_ms", "frames", "first_text_ms", "last_frame_ms",
              "prompt_bytes", "response_bytes", "thought_bytes", "error"):
        if k in d:
            out[k] = d[k]
    if isinstance(d.get("prompt"), str):
        out["prompt_sha"] = hash_short(d["prompt"])
    resp = d.get("response")
    if isinstance(resp, str):
        out["response_sha"] = hash_short(resp)
        out["_resp_len"] = len(resp)
        out["_resp_service"] = _is_service_message(resp)
        # Booléens de classification (jamais exportés : clés « _ »).
        out["_resp_compaction"] = "<summary>" in resp
        _t = resp.strip()
        if len(_t) > 3000:  # tête + queue, comme le détecteur Go
            _t = _t[:1500] + "\n" + _t[-1500:]
        out["_resp_refusal"] = bool(_REFUSAL_RE.search(_t))
        out["_resp_handoff"] = bool(_HANDOFF_RE.search(_t))
        out["_resp_head"] = resp[:200]
        out["_resp_tail"] = resp[-200:]
    rf = d.get("raw_frames")
    if isinstance(rf, list):
        out["tool_call_blocks"], out["tool_names"] = scan_tool_calls(rf)
    return out


# ---------------------------------------------------------------------------
# Parse du log principal
# ---------------------------------------------------------------------------

def parse_log(path):
    """Parse proxy.log et corrèle les lignes par request_id.

    Corrélation `upstream_params` / `upstream_frame_shape` : DOUBLE CHEMIN.
      1. chemin primaire — la ligne porte `request_id` (le Go l'ajoute
         désormais, y compris sur les lignes émises par le client Gemini) :
         elle est attachée directement à cette requête ;
      2. chemin de repli (logs antérieurs sans `request_id`) — la ligne est
         ignorée ici et `build_request` retombe sur les métadonnées de la
         trace (`traces/*.json`, dernier essai) pour thinking_level / mode /
         model_id / first_text_ms. Comportement historique conservé à
         l'identique : aucune re-corrélation heuristique par horodatage
         n'est tentée (risque d'attribution erronée).
    Les compteurs `corr` exposent lequel des deux chemins a servi.
    """
    requests = collections.defaultdict(lambda: {"events": [], "trace": {}})
    markers = []
    total = 0
    corr = {
        "upstream_params_with_request_id": 0,
        "upstream_params_orphan": 0,
        "frame_shape_with_request_id": 0,
        "frame_shape_orphan": 0,
    }
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            total += 1
            if "démarrage" in line or "redémarrage" in line:
                markers.append(line)
                continue
            parsed = parse_slog_line(line)
            if parsed is None:
                continue
            ts, level, msg, attrs = parsed
            rid = attrs.get("request_id")
            if not rid:
                # Repli : ligne sans request_id (logs < K36) -> non attachée,
                # build_request utilisera les métadonnées de la trace.
                if msg == "upstream_params":
                    corr["upstream_params_orphan"] += 1
                elif msg == "upstream_frame_shape":
                    corr["frame_shape_orphan"] += 1
                continue
            r = requests[rid]
            r["events"].append({"ts": ts, "level": level, "msg": msg, "attrs": attrs})
            if msg == "request_start":
                r["time_start"] = ts
                r["method"] = attrs.get("method")
                r["route"] = attrs.get("route")
            elif msg == "request_end":
                r["time_end"] = ts
                for k in ("status", "duration_ms", "bytes", "resp_bytes",
                          "prompt_bytes", "upstream_req_bytes", "upstream_attempts",
                          "upstream_retries", "retry_wait_ms", "first_byte_ms",
                          "upstream_ms", "upstream_resp_bytes", "echo_stripped",
                          "upstream_refusal", "keepalives", "model", "stream"):
                    if k in attrs:
                        r[k] = attrs[k]
            elif msg == "request_params":
                for k in ("messages", "tools", "images", "api", "scalars"):
                    if k in attrs:
                        r[k] = attrs[k]
            elif msg == "upstream_params":
                r.setdefault("upstream_params", []).append(attrs)
                corr["upstream_params_with_request_id"] += 1
            elif msg == "served_model":
                r.setdefault("served_model", []).append(attrs)
            elif msg == "upstream_frame_shape":
                r.setdefault("frame_shapes", []).append(attrs)
                corr["frame_shape_with_request_id"] += 1
    binaries = parse_binary_timeline(markers)
    return requests, binaries, total, corr


# ---------------------------------------------------------------------------
# Classification de l'issue finale
# ---------------------------------------------------------------------------

OUTCOME_PRIORITY = [
    "no_content_timeout", "tool_refusal", "canned_error", "glitch",
    "client_cancelled", "stall_exhausted", "initial_silence_exhausted",
    "quota_pause_blocked", "empty", "error", "tool_calls_ok",
    "text_only_suspect", "text_only_legit",
]

# Échecs durs (jamais comptés dans le taux de succès).
HARD_FAILURE_OUTCOMES = (
    "no_content_timeout", "tool_refusal", "canned_error", "glitch",
    "client_cancelled", "stall_exhausted", "initial_silence_exhausted",
    "empty", "error",
)

# Événements qui trahissent un texte suspect non intercepté.
_SUSPECT_EVENTS = (
    "post_stall_handoff_retry",   # bilan d'étapes détecté puis relancé
    "unverified_action_claim",    # action affirmée sans appel d'outil
    "upstream_tool_refusal_released",  # refus d'outil finalement relayé
)


def _has_text_reply(req):
    """Vrai si la requête a produit du texte exploitable (trace ou tool_calls)."""
    trace = req.get("trace", {}) or {}
    if (trace.get("tool_call_blocks") or 0) > 0:
        return True
    return (trace.get("_resp_len") or 0) > 0


def _text_only_outcome(req):
    """Départage text_only_legit / text_only_suspect.

    Suspect si :
      * la réponse est une compaction `<summary>` -> LEGIT (priorité absolue) ;
      * des outils étaient déclarés et la réponse ne contient AUCUN bloc
        tool_call (et le texte de réponse est connu) ;
      * événement post_stall_handoff_retry / unverified_action_claim /
        upstream_tool_refusal_released ;
      * la réponse matche les motifs refus/service/handoff du fichier.
    Sinon LEGIT (réponse texte attendue, sans signal de refus).
    """
    trace = req.get("trace", {}) or {}
    if trace.get("_resp_compaction"):
        return "text_only_legit"
    if (trace.get("tool_call_blocks") or 0) > 0:
        return "text_only_legit"
    tools = _i(req, "tools", 0) or 0
    resp_known = "_resp_len" in trace
    if tools > 0 and resp_known:
        return "text_only_suspect"   # outils déclarés, aucun tool_call émis
    for e in req.get("events", []):
        if e.get("msg") in _SUSPECT_EVENTS:
            return "text_only_suspect"
    if trace.get("_resp_refusal") or trace.get("_resp_handoff") or trace.get("_resp_service"):
        return "text_only_suspect"
    return "text_only_legit"


_INTERNAL_CANCEL_CAUSES = (
    "no_content_timeout",
    "initial_silence",
    "canned_retry_timeout",
)


def classify(req):
    ev = req.get("events", [])
    trace = req.get("trace", {}) or {}
    tc = trace.get("tool_call_blocks", 0) or 0
    kinds = set()
    has_code_ref_fastpath = False
    for e in ev:
        m, a = e.get("msg"), e.get("attrs", {})
        if m == "upstream_no_content_timeout":
            kinds.add("no_content_timeout")
        elif m == "upstream_tool_refusal" and a.get("action") == "reported":
            kinds.add("tool_refusal")
        elif m == "upstream_canned_error" and a.get("action") in ("reported", "off", "skipped_recent_failures"):
            kinds.add("canned_error")
        elif m == "upstream_glitch" and a.get("action") == "reported":
            kinds.add("glitch")
        elif m == "upstream_empty_reply":
            kinds.add("empty")
        elif m == "client_cancelled" and a.get("cause") not in _INTERNAL_CANCEL_CAUSES:
            kinds.add("client_cancelled")
        elif m == "quota_pause_blocked":
            kinds.add("quota_pause_blocked")
        elif m == "code_ref_fastpath":
            has_code_ref_fastpath = True
    if tc > 0 or (has_code_ref_fastpath and not trace.get("error") and req.get("status") in (None, "", "200")):
        kinds.add("tool_calls_ok")
    if not kinds:
        reasons = [e.get("attrs", {}).get("reason") for e in ev if e.get("msg") == "early_stop"]
        if "initial_silence" in reasons and not _has_text_reply(req):
            kinds.add("initial_silence_exhausted")
        elif "stall" in reasons:
            kinds.add("stall_exhausted")
        elif trace.get("error"):
            kinds.add("error")
        elif req.get("status") == "200":
            kinds.add(_text_only_outcome(req))
        else:
            kinds.add("error")
    for k in OUTCOME_PRIORITY:
        if k in kinds:
            return k
    return "unknown"


# ---------------------------------------------------------------------------
# Agrégation par requête
# ---------------------------------------------------------------------------

def _i(attrs, key, default=None):
    try:
        return int(attrs.get(key, default))
    except (TypeError, ValueError):
        return default


def build_request(rid, req, binaries):
    trace = req.get("trace", {})
    up = req.get("upstream_params", [])
    sv = req.get("served_model", [])
    fs = req.get("frame_shapes", [])

    out = {
        "req_id": rid,
        "binary": binary_at(binaries, parse_iso_ts(req.get("time_start"))),
        "time_start": req.get("time_start", ""),
        "time_end": req.get("time_end", ""),
        "method": req.get("method", ""),
        "route": req.get("route", ""),
        "model": req.get("model", ""),
        "status": req.get("status", ""),
        "duration_ms": _i(req, "duration_ms"),
        "prompt_bytes": _i(req, "prompt_bytes"),
        "upstream_req_bytes": _i(req, "upstream_req_bytes"),
        "resp_bytes": _i(req, "resp_bytes"),
        "upstream_resp_bytes": _i(req, "upstream_resp_bytes"),
        "messages": _i(req, "messages"),
        "tools": _i(req, "tools"),
        "images": _i(req, "images"),
        "attempts": _i(req, "upstream_attempts"),
        "upstream_retries": _i(req, "upstream_retries"),
        "retry_wait_ms": _i(req, "retry_wait_ms"),
        "first_byte_ms": _i(req, "first_byte_ms"),
        "upstream_ms": _i(req, "upstream_ms"),
        "echo_stripped": _i(req, "echo_stripped"),
        "upstream_refusal": req.get("upstream_refusal", ""),
    }

    def pick(lst, key):
        vals = [_i(a, key) for a in lst if key in a]
        return vals[0] if vals else None

    def pick_str(lst, key):
        # pour les identifiants opaques (model_id est hexadécimal, pas un entier)
        vals = [a.get(key) for a in lst if a.get(key) not in (None, "")]
        return vals[0] if vals else None

    out["thinking_level_first"] = pick(up, "thinking_level")
    out["thinking_level_last"] = pick(list(reversed(up)), "thinking_level")
    out["mode_first"] = pick(up, "mode")
    out["mode_last"] = pick(list(reversed(up)), "mode")
    out["model_id"] = pick_str(up, "model_id")
    out["temp_chat_first"] = pick(up, "temporary_chat")
    out["temp_chat_any_retry"] = any(str(a.get("temporary_chat")).lower() == "true" for a in up)
    # Repli (logs sans request_id sur upstream_params) : la trace (dernier
    # essai) porte mode/level/model_id. Depuis K36, upstream_params peut
    # porter request_id : dans ce cas `up` est renseigné et le log gagne
    # (priorité au log, la trace ne sert que de repli — cf. parse_log).
    if out["thinking_level_last"] is None:
        out["thinking_level_last"] = trace.get("thinking_level")
    if out["thinking_level_first"] is None:
        out["thinking_level_first"] = trace.get("thinking_level")
    if out["mode_last"] is None:
        out["mode_last"] = trace.get("mode")
    if out["mode_first"] is None:
        out["mode_first"] = trace.get("mode")
    if not out["model_id"]:
        out["model_id"] = trace.get("model_id")

    if sv:
        last = sv[-1]
        out["served_id"] = last.get("served_id", "")
        out["served_name"] = last.get("served_name", "")
        out["served_identity"] = last.get("identity", "")
    else:
        out["served_id"] = out["served_name"] = out["served_identity"] = ""

    out["first_text_ms"] = trace.get("first_text_ms", _i(fs[-1], "first_text_ms") if fs else None)
    out["last_frame_ms"] = trace.get("last_frame_ms")
    out["frames"] = trace.get("frames")
    out["thought_bytes"] = trace.get("thought_bytes")
    out["response_bytes"] = trace.get("response_bytes")
    out["prompt_sha"] = trace.get("prompt_sha", "")
    out["response_sha"] = trace.get("response_sha", "")
    out["tool_call_blocks"] = trace.get("tool_call_blocks", 0)
    out["tool_names"] = trace.get("tool_names", {})

    counts = collections.Counter()
    reasons = []
    stepdowns = []
    hints = 0
    served_doubt = []
    unparsed_reasons = []
    for e in req.get("events", []):
        m = e.get("msg")
        a = e.get("attrs", {})
        if m:
            counts[m] += 1
        if m == "early_stop":
            reasons.append(a.get("reason", ""))
        if a.get("level_stepdown"):
            stepdowns.append(a.get("level_stepdown"))
        if m == "held_stall_retry" and a.get("retry_hint") == "true":
            hints += 1
        if m == "served_model_doubt":
            served_doubt.append(f"{a.get('served_name','')}({a.get('served_id','')})")
        if m == "tool_call_unparsed":
            unparsed_reasons.append(a.get("reason", ""))

    out["num_early_stop"] = counts["early_stop"]
    out["early_stop_reasons"] = ",".join(reasons)
    out["num_held_stall_retry"] = counts["held_stall_retry"]
    out["num_glitch"] = counts["upstream_glitch"]
    out["num_canned_error"] = counts["upstream_canned_error"]
    out["num_tool_refusal"] = counts["upstream_tool_refusal"]
    out["num_tool_unparsed"] = counts["tool_call_unparsed"]
    out["num_json_repaired"] = counts["tool_call_json_repaired"]
    out["num_served_doubt"] = counts["served_model_doubt"]
    out["num_no_content_timeout"] = counts["upstream_no_content_timeout"]
    out["num_silence_after_thoughts"] = counts["upstream_silence_after_thoughts"]
    out["num_initial_silence_retry"] = counts["initial_silence_retry"]
    out["num_post_stall_handoff_retry"] = counts["post_stall_handoff_retry"]
    out["num_served_model_doubt_retry"] = counts["served_model_doubt_retry"]
    out["num_unverified_claim"] = counts["unverified_action_claim"]
    # K35/K36 : refus d'outil relayé à l'aval, et stall survenu APRÈS la
    # libération du refus (deux signaux distincts, jamais fusionnés).
    out["num_tool_refusal_released"] = counts["upstream_tool_refusal_released"]
    out["num_stall_after_release"] = counts["stall_after_release"]
    out["num_code_ref_fastpath"] = counts["code_ref_fastpath"]
    out["level_stepdown"] = ",".join(stepdowns)
    out["retry_hint_used"] = hints > 0
    out["served_doubt_models"] = ";".join(served_doubt)
    out["unparsed_reasons"] = ";".join(unparsed_reasons)
    out["error"] = trace.get("error", "")

    out["prompt_bucket"] = _bucket(out["prompt_bytes"] or 0)
    out["outcome"] = classify(req)
    return out


def _bucket(size):
    for label, limit in PROMPT_BUCKETS:
        if limit is None or size < limit:
            return label
    return ">600K"


# ---------------------------------------------------------------------------
# Filtres
# ---------------------------------------------------------------------------

CHAT_ROUTE_PREFIXES = (
    "/v1/chat/completions", "/v1/responses", "/v1/messages",
)


def is_chat_request(row):
    """Une requête de génération (exclut favicon, /_status, /_ui, /v1/models…)."""
    rt = row.get("route") or ""
    if row.get("method") != "POST":
        return False
    if rt.startswith(CHAT_ROUTE_PREFIXES):
        return True
    return "generateContent" in rt or "generate" in rt


def filter_requests(requests, args, binaries):
    out = []
    for rid, req in requests.items():
        row = build_request(rid, req, binaries)
        if not is_chat_request(row):
            continue
        ts = parse_iso_ts(row["time_start"] or row["time_end"])
        if ts and args.since and ts < args.since:
            continue
        if ts and args.until and ts > args.until:
            continue
        if args.binary and args.binary not in row["binary"]:
            continue
        if args.model and args.model not in (row["model"] or ""):
            continue
        if args.req_id and args.req_id not in rid:
            continue
        out.append(row)
    return out


# ---------------------------------------------------------------------------
# Résumé agrégé
# ---------------------------------------------------------------------------

SUCCESS_OUTCOMES = {"tool_calls_ok", "text_only_legit"}

RETRY_COLS = {
    "stall": "num_held_stall_retry",
    "glitch": "num_glitch",
    "canned_error": "num_canned_error",
    "tool_refusal": "num_tool_refusal",
    "tool_refusal_released": "num_tool_refusal_released",
    "post_stall_handoff": "num_post_stall_handoff_retry",
    "served_model_doubt": "num_served_model_doubt_retry",
    "initial_silence": "num_initial_silence_retry",
}


def classification_breakdown(outcomes):
    """Répartition lisible : tool_calls_ok / text_only_legit / text_only_suspect
    / échecs durs (avec détail par libellé)."""
    hard_detail = {k: outcomes[k] for k in HARD_FAILURE_OUTCOMES if outcomes.get(k)}
    unknown = outcomes.get("unknown", 0)
    b = {
        "tool_calls_ok": outcomes.get("tool_calls_ok", 0),
        "text_only_legit": outcomes.get("text_only_legit", 0),
        "text_only_suspect": outcomes.get("text_only_suspect", 0),
        "hard_failures": sum(hard_detail.values()),
        "hard_failure_detail": hard_detail,
        "unknown": unknown,
        "total": sum(outcomes.values()),
    }
    b["success"] = b["tool_calls_ok"] + b["text_only_legit"]
    b["success_rate"] = (100.0 * b["success"] / b["total"]) if b["total"] else 0.0
    return b


def aggregate(rows):
    n = len(rows)
    by_model = collections.defaultdict(lambda: [0, 0, 0])
    by_served_model = collections.defaultdict(lambda: [0, 0])
    by_bucket = collections.defaultdict(lambda: [0, 0, 0])
    by_binary = collections.defaultdict(lambda: [0, 0, 0])
    by_tool = collections.Counter()
    outcomes = collections.Counter()
    ttfts = []
    durations = []
    first_bytes = []
    upstream_durs = []
    prompt_sizes = []
    stall_durs = []
    retry_types = collections.defaultdict(lambda: [0, 0])

    for r in rows:
        m = r["model"] or "(none)"
        sm = r.get("served_name") or "(unknown)"
        b = r["prompt_bucket"]
        bi = r["binary"]
        o = r["outcome"]
        outcomes[o] += 1
        ok = o in SUCCESS_OUTCOMES
        tc = o == "tool_calls_ok"
        for d in (by_model[m], by_bucket[b], by_binary[bi]):
            d[0] += 1
        by_served_model[sm][0] += 1
        if ok:
            by_model[m][1] += 1
            by_bucket[b][1] += 1
            by_binary[bi][1] += 1
            by_served_model[sm][1] += 1
        if tc:
            by_model[m][2] += 1
            by_bucket[b][2] += 1
            by_binary[bi][2] += 1
        for nm in (r.get("tool_names") or []):
            by_tool[nm] += 1
        if r["first_text_ms"] is not None:
            try:
                v = int(r["first_text_ms"])
                if v >= 0:
                    ttfts.append(v)
            except (TypeError, ValueError):
                pass
        if r["duration_ms"] is not None:
            try:
                durations.append(int(r["duration_ms"]))
            except (TypeError, ValueError):
                pass
        if r.get("first_byte_ms") is not None:
            try:
                v = int(r["first_byte_ms"])
                if v >= 0:
                    first_bytes.append(v)
            except (TypeError, ValueError):
                pass
        if r.get("upstream_ms") is not None:
            try:
                v = int(r["upstream_ms"])
                if v >= 0:
                    upstream_durs.append(v)
            except (TypeError, ValueError):
                pass
        if r.get("prompt_bytes") is not None:
            try:
                v = int(r["prompt_bytes"])
                if v >= 0:
                    prompt_sizes.append(v)
            except (TypeError, ValueError):
                pass
        reasons = (r.get("early_stop_reasons") or "").split(",")
        if "stall" in reasons and r["duration_ms"] is not None:
            try:
                stall_durs.append(int(r["duration_ms"]))
            except (TypeError, ValueError):
                pass
        for kind, col in RETRY_COLS.items():
            if r.get(col, 0) > 0:
                retry_types[kind][0] += 1
                if ok:
                    retry_types[kind][1] += 1

    def pct(pair):
        return (100.0 * pair[1] / pair[0]) if pair[0] else 0.0

    return {
        "exporter_version": VERSION,
        "generated_at": dt.datetime.now().astimezone().isoformat(timespec="seconds"),
        "total_requests": n,
        "success_rate": (100.0 * sum(1 for r in rows if r["outcome"] in SUCCESS_OUTCOMES) / n) if n else 0.0,
        "outcomes": dict(outcomes),
        "classification": classification_breakdown(outcomes),
        "by_model": {k: {"total": v[0], "success": v[1], "tool_calls": v[2], "success_rate": pct(v)}
                      for k, v in sorted(by_model.items())},
        "by_served_model": {k: {"count": v[0], "success_count": v[1], "success_rate": pct(v)}
                            for k, v in sorted(by_served_model.items())},
        "by_prompt_bucket": {k: {"total": v[0], "success": v[1], "tool_calls": v[2], "success_rate": pct(v)}
                              for k, v in sorted(by_bucket.items())},
        "by_binary": {k: {"total": v[0], "success": v[1], "tool_calls": v[2], "success_rate": pct(v)}
                       for k, v in sorted(by_binary.items())},
        "by_tool": {k: {"requests": v} for k, v in sorted(by_tool.items())},
        "ttft_ms": _percentiles(ttfts),
        "duration_ms": _percentiles(durations),
        "first_byte_ms": _percentiles(first_bytes),
        "upstream_ms": _percentiles(upstream_durs),
        "prompt_bytes": _percentiles(prompt_sizes),
        "stall_requests": len(stall_durs),
        "stall_duration_ms": _percentiles(stall_durs),
        "retry_recovery": {k: {"used": v[0], "recovered": v[1], "rate": pct(v)}
                           for k, v in sorted(retry_types.items())},
    }


def _percentiles(vals):
    if not vals:
        return {"n": 0}
    s = sorted(vals)
    n = len(s)
    def p(pct):
        return s[min(n - 1, int(n * pct / 100.0))]
    return {
        "n": n,
        "mean": round(sum(s) / n, 1),
        "p50": p(50),
        "p90": p(90),
        "p95": p(95),
        "p99": p(99),
        "min": s[0],
        "max": s[-1],
    }


# ---------------------------------------------------------------------------
# Incidents groupés
# ---------------------------------------------------------------------------

INCIDENT_KEY = {
    "no_content_timeout": "num_no_content_timeout",
    "canned_error": "num_canned_error",
    "tool_refusal": "num_tool_refusal",
    "tool_refusal_released": "num_tool_refusal_released",
    "stall_after_release": "num_stall_after_release",
    "glitch": "num_glitch",
    "tool_unparsed": "num_tool_unparsed",
    "served_model_doubt": "num_served_doubt",
    "initial_silence_retry": "num_initial_silence_retry",
    "post_stall_handoff_retry": "num_post_stall_handoff_retry",
    "unverified_action_claim": "num_unverified_claim",
}


def group_incidents(rows, max_per=6):
    groups = collections.defaultdict(list)
    for r in rows:
        for cause, key in INCIDENT_KEY.items():
            if (r.get(key, 0) or 0) > 0:
                groups[cause].append(r)
        if r.get("outcome") == "stall_exhausted":
            groups["stall_exhausted"].append(r)
        if r.get("outcome") == "empty":
            groups["empty"].append(r)
        if r.get("error"):
            groups["error"].append(r)
    result = {}
    for cause, rs in sorted(groups.items()):
        result[cause] = {
            "count": len(rs),
            "representative": [
                {"req_id": r["req_id"], "model": r["model"], "prompt_bytes": r["prompt_bytes"],
                 "first_text_ms": r["first_text_ms"], "outcome": r["outcome"],
                 "served_name": r["served_name"], "unparsed_reasons": r["unparsed_reasons"],
                 "level_stepdown": r["level_stepdown"]}
                for r in rs[:max_per]
            ],
        }
    return result


# ---------------------------------------------------------------------------
# Vérification de secrets dans la sortie
# ---------------------------------------------------------------------------

def assert_no_secrets(text, label):
    for pat in SECRET_PATTERNS:
        m = pat.search(text)
        if m:
            raise SystemExit(
                f"ABORT: motif de secret détecté dans {label} (motif {pat.pattern!r}). "
                f"Aucun fichier de sortie n'a été écrit. Fragment (40 c): {m.group(0)[:40]!r}")
    return text


# ---------------------------------------------------------------------------
# Écriture des sorties
# ---------------------------------------------------------------------------

COLUMNS = [
    "req_id", "binary", "time_start", "time_end", "method", "route", "model",
    "status", "outcome", "duration_ms", "prompt_bytes", "prompt_bucket",
    "upstream_req_bytes", "resp_bytes", "upstream_resp_bytes", "messages", "tools",
    "images", "attempts", "upstream_retries", "retry_wait_ms", "first_byte_ms",
    "first_text_ms", "upstream_ms", "frames", "thought_bytes", "response_bytes",
    "thinking_level_first", "thinking_level_last", "mode_first", "mode_last",
    "model_id", "temp_chat_first", "temp_chat_any_retry", "served_id", "served_name",
    "served_identity", "tool_call_blocks", "tool_names", "early_stop_reasons",
    "num_held_stall_retry", "num_glitch", "num_canned_error", "num_tool_refusal",
    "num_tool_unparsed", "num_json_repaired", "num_served_doubt",
    "num_no_content_timeout", "num_silence_after_thoughts", "num_initial_silence_retry",
    "num_post_stall_handoff_retry", "num_served_model_doubt_retry", "num_unverified_claim",
    "num_tool_refusal_released", "num_stall_after_release", "num_code_ref_fastpath",
    "level_stepdown", "retry_hint_used", "served_doubt_models", "unparsed_reasons",
    "prompt_sha", "response_sha", "error",
]


def _cell(v):
    if isinstance(v, (dict, list, tuple)):
        return json.dumps(v, sort_keys=True, ensure_ascii=False)
    if isinstance(v, bool):
        return "true" if v else "false"
    if v is None:
        return ""
    return v


def _excerpts(rows, trace_map, limit):
    if not limit:
        return []
    items = []
    for r in rows:
        tr = trace_map.get(r["req_id"], {})
        if not tr.get("_resp_service"):
            continue
        items.append({
            "req_id": r["req_id"],
            "outcome": r["outcome"],
            "response_len": tr.get("_resp_len"),
            "head": tr.get("_resp_head", ""),
            "tail": tr.get("_resp_tail", ""),
        })
        if len(items) >= limit:
            break
    return items


def write_outputs(rows, summary, incidents, args, trace_map, formats=None):
    """Écrit uniquement les fichiers des formats demandés.

    json -> requests.json + summary.json + incidents.json ; csv -> requests.csv ;
    md -> report.md. `excluded.txt` est TOUJOURS écrit (il documente
    l'anonymisation) et `excerpts.md` l'est dès que --include-excerpts N > 0,
    indépendamment de `formats` (option opt-in).
    """
    formats = parse_formats(formats) if formats is not None else list(FORMATS)
    os.makedirs(args.out, exist_ok=True)
    files = {}

    if "json" in formats:
        jpath = os.path.join(args.out, "requests.json")
        payload = json.dumps(rows, indent=2, ensure_ascii=False, sort_keys=True)
        assert_no_secrets(payload, "requests.json")
        with open(jpath, "w", encoding="utf-8") as f:
            f.write(payload)
        files["requests.json"] = jpath

        spath = os.path.join(args.out, "summary.json")
        spayload = json.dumps(summary, indent=2, ensure_ascii=False, sort_keys=True)
        assert_no_secrets(spayload, "summary.json")
        with open(spath, "w", encoding="utf-8") as f:
            f.write(spayload)
        files["summary.json"] = spath

        ipath = os.path.join(args.out, "incidents.json")
        ipayload = json.dumps(incidents, indent=2, ensure_ascii=False, sort_keys=True)
        assert_no_secrets(ipayload, "incidents.json")
        with open(ipath, "w", encoding="utf-8") as f:
            f.write(ipayload)
        files["incidents.json"] = ipath

    if "csv" in formats:
        cpath = os.path.join(args.out, "requests.csv")
        buf = io.StringIO()
        w = csv.writer(buf)
        w.writerow(COLUMNS)
        for r in rows:
            w.writerow([_cell(r.get(c)) for c in COLUMNS])
        cpayload = buf.getvalue()
        assert_no_secrets(cpayload, "requests.csv")
        with open(cpath, "w", newline="", encoding="utf-8") as f:
            f.write(cpayload)
        files["requests.csv"] = cpath

    if "md" in formats:
        rpath = os.path.join(args.out, "report.md")
        md = build_report(rows, summary, incidents, args)
        assert_no_secrets(md, "report.md")
        with open(rpath, "w", encoding="utf-8") as f:
            f.write(md)
        files["report.md"] = rpath

    if args.include_excerpts:
        exp = _excerpts(rows, trace_map, args.include_excerpts)
        expath = os.path.join(args.out, "excerpts.md")
        body = "\n\n".join(
            f"### {e['req_id']} ({e['outcome']}, {e['response_len']} c)\n\n"
            f"début: {e['head']}\n\nfin: {e['tail']}" for e in exp)
        assert_no_secrets(body, "excerpts.md")
        with open(expath, "w", encoding="utf-8") as f:
            f.write("# Extraits de réponses upstream non sensibles (service messages)\n\n" + body + "\n")
        files["excerpts.md"] = expath

    excl = [
        "prompt / response / thought / raw_frames : contenu EXCLU (tailles + hashes sha256 tronqués seulement).",
        "extraits de réponse upstream : désactivés par défaut (--include-excerpts N limite aux service messages courts, 200 c début/fin).",
        "aucun cookie, clé API, token ni contenu de fichier utilisateur n'est exporté.",
    ]
    expath = os.path.join(args.out, "excluded.txt")
    with open(expath, "w", encoding="utf-8") as f:
        f.write("\n".join(excl) + "\n")
    files["excluded.txt"] = expath
    return files


def build_report(rows, summary, incidents, args):
    L = ["# Rapport de diagnostic GMINI (anonymisé)", ""]
    L.append(f"- Requêtes analysées : **{summary['total_requests']}**")
    L.append(f"- Taux de succès global (tool_calls_ok + text_only_legit) : **{summary['success_rate']:.1f}%**")
    L.append(f"- Filtres : since={args.since or '-'} until={args.until or '-'} binary={args.binary or '-'} model={args.model or '-'} req_id={args.req_id or '-'}")
    cls = summary.get("classification") or {}
    if cls:
        tot = cls.get("total") or 0

        def _pc(v):
            return (100.0 * v / tot) if tot else 0.0

        L += ["", "## Classification réelle", "",
              "| Groupe | Nombre | % |", "|---|---|---|"]
        L.append(f"| tool_calls_ok (appels d'outils valides) | {cls.get('tool_calls_ok', 0)} | {_pc(cls.get('tool_calls_ok', 0)):.1f}% |")
        L.append(f"| text_only_legit (texte légitime, compaction, réponse attendue) | {cls.get('text_only_legit', 0)} | {_pc(cls.get('text_only_legit', 0)):.1f}% |")
        L.append(f"| text_only_suspect (refus / handoff / aucun outil malgré outils déclarés) | {cls.get('text_only_suspect', 0)} | {_pc(cls.get('text_only_suspect', 0)):.1f}% |")
        L.append(f"| échecs durs (total) | {cls.get('hard_failures', 0)} | {_pc(cls.get('hard_failures', 0)):.1f}% |")
        for k, v in sorted((cls.get("hard_failure_detail") or {}).items(), key=lambda x: -x[1]):
            L.append(f"| ↳ {k} | {v} | {_pc(v):.1f}% |")
        if cls.get("unknown"):
            L.append(f"| inclassables | {cls['unknown']} | {_pc(cls['unknown']):.1f}% |")
        L.append("")
        L.append(f"Succès retenus (tool_calls_ok + text_only_legit) : **{cls.get('success', 0)}/{tot}** = **{cls.get('success_rate', 0.0):.1f}%** (text_only_suspect exclu).")
    L += ["", "## Issues finales", "", "| Issue | Nombre |", "|---|---|"]
    for k, v in sorted(summary["outcomes"].items(), key=lambda x: -x[1]):
        L.append(f"| {k} | {v} |")
    L += ["", "## Succès par modèle", "", "| Modèle | Total | Succès | Tool_calls | Taux |", "|---|---|---|---|---|"]
    for k, v in sorted(summary["by_model"].items()):
        L.append(f"| {k} | {v['total']} | {v['success']} | {v['tool_calls']} | {v['success_rate']:.1f}% |")
    L += ["", "## Succès par tranche de taille de prompt", "", "| Tranche | Total | Succès | Tool_calls | Taux |", "|---|---|---|---|---|"]
    for k, v in summary["by_prompt_bucket"].items():
        L.append(f"| {k} | {v['total']} | {v['success']} | {v['tool_calls']} | {v['success_rate']:.1f}% |")
    L += ["", "## Succès par binaire", "", "| Binaire | Total | Succès | Tool_calls | Taux |", "|---|---|---|---|---|"]
    for k, v in sorted(summary["by_binary"].items()):
        L.append(f"| {k} | {v['total']} | {v['success']} | {v['tool_calls']} | {v['success_rate']:.1f}% |")
    tt, du = summary["ttft_ms"], summary["duration_ms"]
    L += ["", "## TTFT et durée (ms)", ""]
    L.append(f"- TTFT : n={tt.get('n',0)} p50={tt.get('p50')} p90={tt.get('p90')} p95={tt.get('p95')} p99={tt.get('p99')} min={tt.get('min')} max={tt.get('max')}")
    L.append(f"- Durée : n={du.get('n',0)} p50={du.get('p50')} p90={du.get('p90')} p95={du.get('p95')} p99={du.get('p99')} max={du.get('max')}")
    L += ["", "## Récupération par type de retry", "", "| Type retry | Utilisé | Récupéré | Taux |", "|---|---|---|---|"]
    for k, v in sorted(summary["retry_recovery"].items()):
        L.append(f"| {k} | {v['used']} | {v['recovered']} | {v['rate']:.1f}% |")
    L += ["", "## Incidents par cause", ""]
    for cause, v in sorted(incidents.items()):
        L.append(f"### {cause} ({v['count']})")
        L += ["", "| req_id | modèle | prompt_bytes | first_text_ms | issue | served |", "|---|---|---|---|---|---|"]
        for rep in v["representative"]:
            L.append(f"| {rep['req_id']} | {rep['model']} | {rep['prompt_bytes']} | {rep['first_text_ms']} | {rep['outcome']} | {rep['served_name']} |")
        L.append("")
    L += ["## Contenu exclu (anonymisation)", ""]
    L.append("- Aucun contenu de prompt/fichier/réponse n'est exporté (tailles, hashes, types d'événements, noms d'outils uniquement).")
    L.append("- Extraits de réponse upstream désactivés par défaut (--include-excerpts N).")
    return "\n".join(L)


# ---------------------------------------------------------------------------
# Selftest (jeu de logs synthétique)
# ---------------------------------------------------------------------------

def selftest():
    """Jeu de logs/traces synthétiques : pipeline, formats, classification K35/K36,
    corrélation request_id, anonymisation et blocage de secret."""
    import shutil
    d = tempfile.mkdtemp(prefix="gmini_selftest_")
    try:
        # ---- 0. --format : liste, all, sous-ensemble, rejet ------------------
        assert parse_formats("json,csv,md") == ["json", "csv", "md"]
        assert parse_formats("all") == ["json", "csv", "md"]
        assert parse_formats("ALL") == ["json", "csv", "md"]
        assert parse_formats("csv") == ["csv"]
        assert parse_formats("md,json") == ["json", "md"]  # ordre canonique
        assert parse_formats("csv,csv") == ["csv"]         # dédoublonné
        assert parse_formats(None) == ["json", "csv", "md"]
        assert parse_formats(["csv", "md"]) == ["csv", "md"]
        for bad in ("nope", "json,nope", "", ","):
            try:
                parse_formats(bad)
            except SystemExit:
                pass
            else:
                raise AssertionError(f"parse_formats({bad!r}) aurait dû échouer")

        # ---- 0b. --last : parse_duration valide / invalide ---------------
        assert parse_duration("15m") == dt.timedelta(minutes=15)
        assert parse_duration("1h") == dt.timedelta(hours=1)
        assert parse_duration("24h") == dt.timedelta(hours=24)
        assert parse_duration("7d") == dt.timedelta(days=7)
        assert parse_duration("30s") == dt.timedelta(seconds=30)
        for bad_dur in ("", "0h", "-5m", "1w", "abc", "1.5h"):
            try:
                parse_duration(bad_dur)
            except SystemExit:
                pass
            else:
                raise AssertionError(f"parse_duration({bad_dur!r}) aurait dû échouer")
        parsed_cli = build_parser().parse_args(["--last", "1h"])
        assert parsed_cli.last == "1h", parsed_cli.last

        log = os.path.join(d, "proxy.log")
        traces = os.path.join(d, "traces")
        os.makedirs(traces)

        A, B, C, D, E, F, G, H = ("AAAA000000000001", "BBBB000000000002",
                                  "CCCC000000000003", "DDDD000000000004",
                                  "EEEE000000000005", "FFFF000000000006",
                                  "GGGG000000000007", "HHHH000000000008")
        lines = [
            '=== démarrage 2026-11-19 07:28:55 binaire=go-gemini-web2api-p9-0581e87-k36 ===',
            # --- A : succès tool_calls + tous les événements K34/K35/K36 -------
            'time=2026-11-19T07:30:31.780+01:00 level=INFO msg=request_params request_id=%s api=chat scalars="temperature=0.7" messages=10 tools=16 images=0' % A,
            'time=2026-11-19T07:30:31.782+01:00 level=INFO msg=upstream_params request_id=%s thinking_level=4 mode=1 temporary_chat=false model=gemini-everyday-help model_id=56fdd199312815e2' % A,
            'time=2026-11-19T07:30:31.789+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % A,
            'time=2026-11-19T07:31:18.282+01:00 level=WARN msg=early_stop request_id=%s reason=stall tool_calls=0 raw_bytes=821 held_bytes=821 sent_bytes=0 elapsed_ms=46501' % A,
            'time=2026-11-19T07:31:19.721+01:00 level=INFO msg=held_stall_retry request_id=%s attempt=1 max_retries=2 held_bytes=821 fast_stall=true retry_hint=true temp_chat=true' % A,
            'time=2026-11-19T07:31:20.000+01:00 level=WARN msg=initial_silence_retry request_id=%s timeout_sec=30 heartbeats=1 temp_chat=true mode=1 level_stepdown=4->1' % A,
            'time=2026-11-19T07:31:21.000+01:00 level=WARN msg=post_stall_handoff_retry request_id=%s action=retry_once level_stepdown=4->1' % A,
            'time=2026-11-19T07:31:22.000+01:00 level=WARN msg=served_model_doubt request_id=%s model=gemini-everyday-help served_name="4 Neon" served_id=e6fa609c3fa255c0 expected_id=56fdd199312815e2' % A,
            'time=2026-11-19T07:31:23.000+01:00 level=WARN msg=served_model_doubt_retry request_id=%s action=retry_once' % A,
            'time=2026-11-19T07:31:24.000+01:00 level=WARN msg=upstream_tool_refusal_released request_id=%s bytes=1234 sent_bytes=0' % A,
            'time=2026-11-19T07:31:25.000+01:00 level=WARN msg=unverified_action_claim request_id=%s kind=claim_without_tool bytes=99 released=false' % A,
            'time=2026-11-19T07:31:26.000+01:00 level=WARN msg=stall_after_release request_id=%s elapsed_ms=46501 released_bytes=1234' % A,
            'time=2026-11-19T07:31:27.000+01:00 level=WARN msg=tool_call_unparsed request_id=%s reason=missing_fence' % A,
            'time=2026-11-19T07:31:56.789+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-everyday-help stream=true status=200 duration_ms=85055 prompt_bytes=614512 upstream_attempts=2 upstream_retries=0 first_byte_ms=5647 upstream_ms=82253 upstream_resp_bytes=1405854 resp_bytes=13141' % A,
            # --- B : texte sans tool_call alors que 16 outils déclarés -------
            'time=2026-11-19T07:45:01.990+01:00 level=INFO msg=request_params request_id=%s api=chat messages=4 tools=16 images=0' % B,
            'time=2026-11-19T07:45:01.991+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % B,
            'time=2026-11-19T07:45:02.000+01:00 level=INFO msg=upstream_frame_shape request_id=%s frames=3 text_bytes=120 first_text_ms=1234 last_frame_ms=2000' % B,
            'time=2026-11-19T07:45:15.000+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-most-complex-work stream=true status=200 duration_ms=13010 prompt_bytes=80000 upstream_attempts=1 first_byte_ms=3000 upstream_ms=12900 upstream_resp_bytes=5000 resp_bytes=5000' % B,
            # --- C : compaction <summary> (outils déclarés) ------------------
            'time=2026-11-19T07:46:00.000+01:00 level=INFO msg=request_params request_id=%s api=chat messages=22 tools=16 images=0' % C,
            'time=2026-11-19T07:46:00.010+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % C,
            'time=2026-11-19T07:46:05.000+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-everyday-help stream=true status=200 duration_ms=5000 prompt_bytes=120000 upstream_attempts=1 resp_bytes=900' % C,
            # --- D : texte légitime, sans outil ; upstream_params orphelin ----
            'time=2026-11-19T07:47:00.000+01:00 level=INFO msg=upstream_params thinking_level=1 mode=1 temporary_chat=false model=gemini-everyday-help model_id=orphelin00000000',
            'time=2026-11-19T07:47:00.020+01:00 level=INFO msg=request_params request_id=%s api=chat messages=2 tools=0 images=0' % D,
            'time=2026-11-19T07:47:00.030+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % D,
            'time=2026-11-19T07:47:03.000+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-everyday-help stream=true status=200 duration_ms=3000 prompt_bytes=4000 upstream_attempts=1 resp_bytes=400' % D,
            # --- E : refus en texte, aucun outil déclaré ---------------------
            'time=2026-11-19T07:48:00.000+01:00 level=INFO msg=request_params request_id=%s api=chat messages=2 tools=0 images=0' % E,
            'time=2026-11-19T07:48:00.030+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % E,
            'time=2026-11-19T07:48:09.000+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-everyday-help stream=true status=200 duration_ms=9000 prompt_bytes=5000 upstream_attempts=1 resp_bytes=800' % E,
            # --- F : texte neutre + événement unverified_action_claim --------
            'time=2026-11-19T07:49:00.000+01:00 level=INFO msg=request_params request_id=%s api=chat messages=2 tools=0 images=0' % F,
            'time=2026-11-19T07:49:00.030+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % F,
            'time=2026-11-19T07:49:05.000+01:00 level=WARN msg=unverified_action_claim request_id=%s kind=claim_without_tool bytes=42 released=true' % F,
            'time=2026-11-19T07:49:09.000+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-everyday-help stream=true status=200 duration_ms=9000 prompt_bytes=5000 upstream_attempts=1 resp_bytes=300' % F,
            # --- G : silence initial épuisé ----------------------------------
            'time=2026-11-19T07:50:00.000+01:00 level=INFO msg=request_params request_id=%s api=chat messages=2 tools=4 images=0' % G,
            'time=2026-11-19T07:50:00.030+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % G,
            'time=2026-11-19T07:50:45.000+01:00 level=WARN msg=early_stop request_id=%s reason=initial_silence tool_calls=0 raw_bytes=0 held_bytes=0 sent_bytes=0 elapsed_ms=45000' % G,
            'time=2026-11-19T07:50:46.000+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-everyday-help stream=true status=504 duration_ms=46000 prompt_bytes=5000 upstream_attempts=1 resp_bytes=0' % G,
            # --- H : code_ref_fastpath (outils déclarés, pas de tool_call_blocks) -> tool_calls_ok
            'time=2026-11-19T07:51:00.000+01:00 level=INFO msg=request_params request_id=%s api=chat messages=6 tools=16 images=0' % H,
            'time=2026-11-19T07:51:00.010+01:00 level=INFO msg=request_start request_id=%s method=POST route=/v1/chat/completions' % H,
            'time=2026-11-19T07:51:02.000+01:00 level=INFO msg=served_model request_id=%s served_id=56fdd199312815e2 served_name="3 Pro" identity=expected' % H,
            'time=2026-11-19T07:51:03.000+01:00 level=INFO msg=code_ref_fastpath request_id=%s tool=run_command' % H,
            'time=2026-11-19T07:51:04.000+01:00 level=INFO msg=request_end request_id=%s method=POST route=/v1/chat/completions model=gemini-everyday-help stream=true status=200 duration_ms=4000 prompt_bytes=12000 upstream_attempts=1 first_byte_ms=1100 upstream_ms=3800 resp_bytes=450' % H,
        ]
        with open(log, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")

        trace_defs = [
            (A, {"thinking_level": 2, "mode": 2, "first_text_ms": 20274,   # valeurs FAUSSES :
                 "response": "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/x\"}}\n```",  # le log
                 "raw_frames": ['```tool_call\n{"name":"read_file","arguments":{"path":"/x"}}\n```']}),  # doit gagner
            (B, {"thinking_level": 2, "mode": 1,
                 "response": "Voici les trois options demandées."}),  # pas de first_text_ms
            (C, {"thinking_level": 4, "mode": 1,
                 "response": "<summary>Résumé de compaction du contexte.</summary>"}),
            (D, {"thinking_level": 3, "mode": 1,
                 "response": "Voici le résultat attendu en texte simple."}),
            (E, {"thinking_level": 2, "mode": 1,
                 "response": "I'm having a hard time fulfilling your request without tools right now."}),
            (F, {"thinking_level": 2, "mode": 1, "response": "Tout est terminé."}),
            (H, {"thinking_level": 2, "mode": 1, "response": "```bash\necho ok\n```"}),
        ]
        trace_paths = {}
        for i, (rid, extra) in enumerate(trace_defs):
            tr = {"time": "2026-11-19T07:3%d:00+01:00" % i, "request_id": rid,
                  "model": "gemini-everyday-help", "frames": 4,
                  "prompt_bytes": 5000, "response_bytes": 400, "thought_bytes": 0,
                  "prompt": "x" * 5000}
            tr.update(extra)
            tp = os.path.join(traces, "20261119T07%02d00.%s.json" % (30 + i, rid))
            with open(tp, "w") as f:
                json.dump(tr, f)
            trace_paths[rid] = tp

        requests, binaries, total, corr = parse_log(log)
        assert len(requests) == 8, sorted(requests)
        assert corr["upstream_params_with_request_id"] == 1, corr
        assert corr["upstream_params_orphan"] == 1, corr
        assert corr["frame_shape_with_request_id"] == 1, corr
        for rid, tp in trace_paths.items():
            requests[rid]["trace"] = load_trace(tp)
        assert len(requests[A]["trace"]) > 0, "trace A non chargée"

        rows = [build_request(rid, requests[rid], binaries) for rid in sorted(requests)]
        by = {r["req_id"]: r for r in rows}
        ra = by[A]

        # ---- 1. outils + tous les compteurs K34/K35/K36 ------------------
        assert ra["binary"].endswith("k36"), ra["binary"]
        assert ra["tool_call_blocks"] == 1, ra["tool_call_blocks"]
        assert ra["prompt_bucket"] in (">600K", "450-600K"), ra["prompt_bucket"]
        assert ra["outcome"] == "tool_calls_ok", ra["outcome"]
        for col, want in (("num_initial_silence_retry", 1),
                          ("num_post_stall_handoff_retry", 1),
                          ("num_served_doubt", 1),
                          ("num_served_model_doubt_retry", 1),
                          ("num_tool_refusal_released", 1),
                          ("num_unverified_claim", 1),
                          ("num_stall_after_release", 1),
                          ("num_tool_unparsed", 1),
                          ("num_held_stall_retry", 1)):
            assert col in COLUMNS, f"colonne absente : {col}"
            assert ra[col] == want, (col, ra[col], want)
        assert ra["unparsed_reasons"] == "missing_fence", ra["unparsed_reasons"]
        # corrélation : upstream_params AVEC request_id -> valeurs du log
        assert ra["thinking_level_first"] == 4, ra["thinking_level_first"]
        assert ra["mode_first"] == 1, ra["mode_first"]
        assert ra["model_id"] == "56fdd199312815e2", ra["model_id"]

        # ---- 2. classification réelle ------------------------------------
        assert by[C]["outcome"] == "text_only_legit", by[C]["outcome"]   # compaction
        assert by[D]["outcome"] == "text_only_legit", by[D]["outcome"]   # texte simple
        assert by[B]["outcome"] == "text_only_suspect", by[B]["outcome"]  # outils déclarés
        assert by[E]["outcome"] == "text_only_suspect", by[E]["outcome"]  # refus en texte
        assert by[F]["outcome"] == "text_only_suspect", by[F]["outcome"]  # événement
        assert by[G]["outcome"] == "initial_silence_exhausted", by[G]["outcome"]
        assert by[H]["outcome"] == "tool_calls_ok", by[H]["outcome"]  # code_ref_fastpath
        assert by[H]["num_code_ref_fastpath"] == 1, by[H]["num_code_ref_fastpath"]
        assert "num_code_ref_fastpath" in COLUMNS
        # repli trace pour les logs sans request_id (upstream_params orphelin=1
        # ne doit PAS contaminer D, dont la trace dit thinking_level=3)
        assert by[D]["thinking_level_first"] == 3, by[D]["thinking_level_first"]
        # upstream_frame_shape AVEC request_id -> first_text_ms du log
        assert by[B]["first_text_ms"] == 1234, by[B]["first_text_ms"]

        summary = aggregate(rows)
        cls = summary["classification"]
        assert summary["exporter_version"] == VERSION, summary.get("exporter_version")
        assert "generated_at" in summary and "T" in summary["generated_at"], summary.get("generated_at")
        assert summary["total_requests"] == 8, summary["total_requests"]
        assert cls["tool_calls_ok"] == 2, cls
        assert cls["text_only_legit"] == 2, cls
        assert cls["text_only_suspect"] == 3, cls
        assert cls["hard_failures"] == 1, cls
        assert cls["hard_failure_detail"].get("initial_silence_exhausted") == 1, cls
        assert abs(summary["success_rate"] - (100.0 * 4 / 8)) < 0.01, summary["success_rate"]
        assert summary["outcomes"].get("text_only_suspect") == 3, summary["outcomes"]
        assert "tool_calls" not in summary["outcomes"], summary["outcomes"]
        # nouveaux agrégats v1.2.0
        for metric in ("ttft_ms", "duration_ms", "first_byte_ms", "upstream_ms", "prompt_bytes"):
            m = summary[metric]
            assert m.get("n", 0) > 0, (metric, m)
            for k in ("mean", "p50", "p90", "p95", "p99", "min", "max"):
                assert k in m, (metric, k, m)
        assert "3 Pro" in summary["by_served_model"], summary["by_served_model"]
        assert summary["by_served_model"]["3 Pro"]["count"] == 1
        assert summary["by_served_model"]["3 Pro"]["success_count"] == 1
        incidents = group_incidents(rows)
        assert incidents["stall_after_release"]["count"] == 1, incidents.keys()
        assert incidents["tool_refusal_released"]["count"] == 1, incidents.keys()
        assert incidents["unverified_action_claim"]["count"] == 2, incidents.keys()

        # ---- 3. filtres (is_chat_request / bornes) ------------------------
        fa = argparse.Namespace(since=parse_user_dt("2026-11-19 07:27:00"),
                                until=None, binary=None, model=None, req_id=None)
        assert len(filter_requests(requests, fa, binaries)) == 8, "filtre since"
        fa2 = argparse.Namespace(since=None, until=None, binary="k36",
                                 model="gemini-everyday-help", req_id="AAAA")
        assert len(filter_requests(requests, fa2, binaries)) == 1, "filtres combinés"

        # ---- 4. --format : seuls les fichiers demandés sont écrits --------
        trace_map = {rid: requests[rid].get("trace", {}) for rid in requests}
        args = argparse.Namespace(out=os.path.join(d, "out_csv"), since=None, until=None,
                                  binary=None, model=None, req_id=None,
                                  include_excerpts=0, format=None)
        files = write_outputs(rows, summary, incidents, args, trace_map, ["csv"])
        assert set(files) == {"requests.csv", "excluded.txt"}, sorted(files)
        args.out = os.path.join(d, "out_md")
        files = write_outputs(rows, summary, incidents, args, trace_map, ["md"])
        assert set(files) == {"report.md", "excluded.txt"}, sorted(files)
        with open(files["report.md"], encoding="utf-8") as f:
            md = f.read()
        assert "## Classification réelle" in md, "section classification absente"
        assert "## Issues finales" in md, "section issues absente"

        args.out = os.path.join(d, "out_all")
        files = write_outputs(rows, summary, incidents, args, trace_map,
                             parse_formats("all"))
        assert set(files) == {"requests.json", "requests.csv", "summary.json",
                              "incidents.json", "report.md", "excluded.txt"}, sorted(files)
        # idempotence : seconde exécution -> octets identiques
        first = open(files["requests.csv"], encoding="utf-8").read()
        assert "num_code_ref_fastpath" in first.splitlines()[0], "colonne num_code_ref_fastpath absente du CSV"
        write_outputs(rows, summary, incidents, args, trace_map, parse_formats("all"))
        assert open(files["requests.csv"], encoding="utf-8").read() == first, "non idempotent"
        with open(files["summary.json"], encoding="utf-8") as f:
            sj = json.load(f)
        assert sj["classification"]["text_only_suspect"] == 3, sj["classification"]

        # excerpts.md : produit dès que --include-excerpts N > 0, même en json seul
        args.out = os.path.join(d, "out_exc")
        args.include_excerpts = 1
        files = write_outputs(rows, summary, incidents, args, trace_map, ["json"])
        assert "excerpts.md" in files, sorted(files)
        assert set(files) == {"requests.json", "summary.json", "incidents.json",
                              "excluded.txt", "excerpts.md"}, sorted(files)

        # ---- 5. anonymisation / blocage de secret ------------------------
        with open(files["requests.json"], encoding="utf-8") as f:
            payload = f.read()
        for forbidden in ("Voici les trois options demandées.",
                          "I'm having a hard time fulfilling",
                          "Résumé de compaction du contexte",
                          "x" * 200):
            assert forbidden not in payload, f"contenu de réponse exporté : {forbidden[:20]!r}"
        assert_no_secrets("rien de suspect", "test")
        for secret in ("clé: AIzaSyD123456789012345678901234",
                       "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345",
                       "api_key=supersecretvalue1234"):
            try:
                assert_no_secrets(secret, "test")
            except SystemExit as exc:
                assert "ABORT" in str(exc), exc
            else:
                raise AssertionError(f"secret non détecté : {secret[:24]!r}")
        # assert_no_secrets sur summary.json et incidents.json
        bad_summary = dict(summary, note="AIzaSyD123456789012345678901234")
        bad_args = argparse.Namespace(out=os.path.join(d, "out_bad_sum"), since=None, until=None,
                                      binary=None, model=None, req_id=None,
                                      include_excerpts=0, format=None)
        try:
            write_outputs(rows, bad_summary, incidents, bad_args, trace_map, ["json"])
        except SystemExit as exc:
            assert "summary.json" in str(exc), exc
        else:
            raise AssertionError("secret dans summary.json non bloqué")
        bad_incidents = dict(incidents, leak={"count": 1, "representative": [{"req_id": "sk-123456789012345678901234"}]})
        bad_args.out = os.path.join(d, "out_bad_inc")
        try:
            write_outputs(rows, summary, bad_incidents, bad_args, trace_map, ["json"])
        except SystemExit as exc:
            assert "incidents.json" in str(exc), exc
        else:
            raise AssertionError("secret dans incidents.json non bloqué")
        print(f"selftest OK (v{VERSION}) — formats, --last, classification K35/K36 + code_ref_fastpath, "
              "corrélation request_id, agrégats étendus, anonymisation et détection de secret validés.")
    finally:
        shutil.rmtree(d, ignore_errors=True)


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def build_parser():
    p = argparse.ArgumentParser(
        description="Export anonymisé des diagnostics du proxy GMINI (proxy.log + traces/).",
        formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--log", default=DEFAULT_LOG, help=f"Chemin proxy.log (défaut {DEFAULT_LOG})")
    p.add_argument("--traces", default=DEFAULT_TRACES, help=f"Dossier traces/ (défaut {DEFAULT_TRACES})")
    p.add_argument("--since", help="Borne basse (AAAA-MM-JJ[THH:MM:SS] ou HH:MM)")
    p.add_argument("--until", help="Borne haute (AAAA-MM-JJ[THH:MM:SS] ou HH:MM)")
    p.add_argument("--last", help="Fenêtre relative depuis maintenant (ex. 15m, 1h, 6h, 24h, 7d) ; calcule --since si omis")
    p.add_argument("--binary", help="Filtrer par sous-chaîne de nom de binaire (ex. k35)")
    p.add_argument("--model", help="Filtrer par nom de modèle demandé (ex. gemini-everyday-help)")
    p.add_argument("--req-id", help="Filtrer par request_id (ou préfixe)")
    p.add_argument("--out", default="./gmini_diagnostics", help="Dossier de sortie")
    p.add_argument("--format", type=parse_formats, default=None, metavar="LISTE",
                   help="Formats à produire, séparés par des virgules : "
                        "json,csv,md — tout sous-ensemble, ou 'all' (= json,csv,md). "
                        "Défaut : tous. Effets : json -> requests.json + summary.json "
                        "+ incidents.json ; csv -> requests.csv ; md -> report.md. "
                        "excluded.txt est toujours écrit ; excerpts.md suit "
                        "--include-excerpts.")
    p.add_argument("--include-excerpts", type=int, default=0, metavar="N",
                   help="Inclure jusqu'à N extraits (200 c début/fin) de réponses upstream NON sensibles (0=désactivé)")
    p.add_argument("--selftest", action="store_true", help="Lancer le test synthétique et quitter")
    p.add_argument("--version", action="version", version=f"%(prog)s {VERSION}")
    return p


def main(argv=None):
    args = build_parser().parse_args(argv)
    if args.selftest:
        selftest()
        return 0

    last_delta = parse_duration(args.last) if args.last is not None else None
    args.since = parse_user_dt(args.since, end_of_day=False)
    if args.since is None and last_delta is not None:
        args.since = dt.datetime.now().replace(microsecond=0) - last_delta
    args.until = parse_user_dt(args.until, end_of_day=True)
    formats = parse_formats(args.format)

    if not os.path.exists(args.log):
        raise SystemExit(f"proxy.log introuvable : {args.log}")

    print(f"[1/4] Parse de {args.log} ...")
    requests, binaries, total, corr = parse_log(args.log)
    print(f"      {total} lignes, {len(requests)} requêtes, {len(binaries)} démarrages de binaire.")
    print(f"      corrélation : upstream_params {corr['upstream_params_with_request_id']} "
          f"via request_id / {corr['upstream_params_orphan']} sans request_id (repli trace) ; "
          f"frame_shape {corr['frame_shape_with_request_id']} via request_id / "
          f"{corr['frame_shape_orphan']} sans request_id")

    print(f"[2/4] Lecture des traces {args.traces} ...")
    by_rid = collections.defaultdict(list)
    for tp in sorted(glob.glob(os.path.join(args.traces, "*.json"))):
        tr = load_trace(tp)
        rid = tr.get("request_id")
        if rid:
            by_rid[rid].append(tr)
    trace_map = {}
    n_traces = 0
    for rid, trs in by_rid.items():
        trs.sort(key=lambda t: t.get("time", ""))
        last = trs[-1]  # le dernier essai (retry) reflète l'état final
        trace_map[rid] = last
        if rid in requests:
            requests[rid]["trace"] = last
            n_traces += 1
    print(f"      {len(trace_map)} traces (dernier essai) attachées, {n_traces} requêtes couvertes.")

    print("[3/4] Agrégation ...")
    rows = filter_requests(requests, args, binaries)
    rows = sorted(rows, key=lambda r: r["time_start"] or "")
    summary = aggregate(rows)
    summary["correlation"] = corr
    summary["formats"] = list(formats)
    incidents = group_incidents(rows)
    print(f"      {len(rows)} requêtes retenues après filtres.")

    print(f"[4/4] Écriture dans {args.out} (formats: {','.join(formats)}) ...")
    files = write_outputs(rows, summary, incidents, args, trace_map, formats)
    for k in sorted(files):
        print(f"      {k}")

    cls = summary["classification"]
    print("\nRésumé :")
    print(f"  requêtes      : {summary['total_requests']}")
    print(f"  succès global : {summary['success_rate']:.1f}%")
    print(f"  classification: tool_calls_ok={cls['tool_calls_ok']} "
          f"text_only_legit={cls['text_only_legit']} "
          f"text_only_suspect={cls['text_only_suspect']} "
          f"échecs_durs={cls['hard_failures']}")
    print(f"  issues        : {json.dumps(summary['outcomes'], sort_keys=True)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
