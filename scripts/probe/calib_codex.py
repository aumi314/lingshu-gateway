# 完整 Codex 伪装下的官方基准和推理题校准，经本机 Magpie 的 codex/*，复现 internal/relay/probe_responses.go 的请求体。
# 基准：python3 scripts/probe/calib_codex.py references/probe-sites/probe-disguise.json gpt-6.2-sol gpt-6.2-astra；结果写进 internal/probe/run.go 的 templateBases。
# 推理题：先 lingshu probe-export-math -seed 123 -count 10 -out docs/research/data/math-seed-123.json，再
#         python3 scripts/probe/calib_codex.py math references/probe-sites/probe-disguise.json gpt-6.2-sol docs/research/data/math-seed-123.json
# 每道题完成即落盘，重跑只补缺；续跑时校验题单 sha256，题单或配置换了就换 --out 文件。
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import time
import urllib.error
import urllib.request
import uuid

NO_TOOLS = "\n\nAnswer directly; do not use any tools."
MAGPIE = "http://127.0.0.1:3425/v1/responses"
EMBEDDED_DATA = Path(__file__).resolve().parents[2] / "internal/probe/data"


def codex_model(model):
    return "codex/" + model.removeprefix("codex/")


def response_events(response):
    for raw in response:
        line = raw.decode("utf-8", "replace").strip()
        if line.startswith("data:") and line[5:].strip() != "[DONE]":
            yield json.loads(line[5:])


def send(d, model, prompt, effort, session, install):
    """发一个完整 Codex 伪装请求，返回正文和用量；429 / 5xx 等 20 秒重试，最多 6 次。"""
    items = json.loads(json.dumps(d["input"]).replace("{{DATE}}", time.strftime("%Y-%m-%d")))
    items.append({"type": "message", "role": "user", "content": [{"type": "input_text", "text": prompt + NO_TOOLS}]})
    turn = str(uuid.uuid4())
    window = session + ":0"
    meta = {"installation_id": install, "session_id": session, "thread_id": session, "agent_name": "/root", "turn_id": turn, "window_id": window, "window_number": 0,
            "context_window_id": str(uuid.uuid4()), "request_kind": "turn", "root_turn_id": turn, "thread_source": "user", "turn_trigger": "exec",
            "sandbox": "seccomp", "sandbox_mode": "read-only", "auto_review_enabled": False, "node_repl_auto_review_required": True,
            "node_repl_disabled": False, "turn_started_at_unix_ms": int(time.time() * 1000), "analytics_enabled": True, "model": model}
    if effort:
        meta["reasoning_effort"] = effort
    turn_meta = json.dumps(meta, separators=(",", ":"))
    body = {"model": model, "stream": True, "store": False, "include": ["reasoning.encrypted_content"], "prompt_cache_key": session,
            **d.get("body", {}), "input": items,
            "client_metadata": {"x-codex-window-id": window, "turn_id": turn, "x-codex-turn-metadata": turn_meta, "thread_id": session,
                                "x-codex-installation-id": install, "root_turn_id": turn, "session_id": session}}
    reasoning = {}
    if effort:
        reasoning["effort"] = effort
    if d.get("reasoning_context"):
        reasoning["context"] = d["reasoning_context"]
    if reasoning:
        body["reasoning"] = reasoning
    headers = {**d["headers"], "Authorization": "Bearer magpie", "Content-Type": "application/json", "Accept": "text/event-stream",
               "X-Codex-Turn-Metadata": turn_meta, "X-Codex-Window-Id": window, "Session-Id": session, "Thread-Id": session, "X-Client-Request-Id": session}
    req = urllib.request.Request(MAGPIE, data=json.dumps(body).encode(), headers=headers)
    for attempt in range(6):
        try:
            text, final, terminal = "", {}, ""
            with urllib.request.urlopen(req, timeout=600) as response:
                for ev in response_events(response):
                    kind = ev.get("type")
                    if kind == "response.output_text.delta":
                        text += ev["delta"]
                    if kind in ("response.completed", "response.incomplete", "response.failed", "error"):
                        terminal = kind
                        final = ev.get("response", {})
            if not text:
                text = "".join(part.get("text", "") for item in final.get("output", []) if item.get("type") == "message"
                               for part in item.get("content", []) if part.get("type") == "output_text")
            return {"text": text, "usage": final.get("usage") or {}, "terminal": terminal,
                    "status": "ok" if terminal == "response.completed" else "failed"}
        except urllib.error.HTTPError as e:
            if e.code not in (408, 429, 500, 502, 503, 504, 524, 529) or attempt == 5:
                raise
            time.sleep(20)


def anchor(args):
    d = json.loads(Path(args.disguise).read_text())["codex"]

    def sample(model):
        items = json.loads(json.dumps(d["input"]).replace("{{DATE}}", time.strftime("%Y-%m-%d")))
        items.append({"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Reply with exactly: ok"}]})
        body = {"model": codex_model(model), "stream": True, "store": False, "include": ["reasoning.encrypted_content"],
                "prompt_cache_key": str(uuid.uuid4()), **d.get("body", {}), "input": items}
        if d.get("reasoning_context"):
            body["reasoning"] = {"context": d["reasoning_context"]}
        req = urllib.request.Request(MAGPIE, data=json.dumps(body).encode(),
                                     headers={"Authorization": "Bearer magpie", "Content-Type": "application/json"})
        text, usage = "", {}
        with urllib.request.urlopen(req, timeout=300) as response:
            for ev in response_events(response):
                if ev.get("type") == "response.output_text.delta":
                    text += ev["delta"]
                if ev.get("type") == "response.completed":
                    usage = ev["response"].get("usage") or {}
        return text, usage.get("input_tokens")

    for model in args.models:
        print(model, [sample(model) for _ in range(2)], flush=True)


def math(args):
    manifest = Path(args.manifest).resolve()
    model = codex_model(args.model)
    output = Path(args.out).resolve() if args.out else manifest.with_name(f"codex-{model.removeprefix('codex/')}-{manifest.name}")
    if output == manifest or output.is_relative_to(EMBEDDED_DATA):
        raise ValueError("结果文件必须与题单分开，且放在 internal/probe/data/ 之外")
    raw = manifest.read_bytes()
    problems = json.loads(raw)["problems"]
    d = json.loads(Path(args.disguise).read_text())["codex"]
    template_hash = hashlib.sha256(json.dumps(d, sort_keys=True).encode()).hexdigest()
    state = {"manifest_sha256": hashlib.sha256(raw).hexdigest(), "model": model,
             "effort": args.effort, "template_sha256": template_hash, "results": {}}
    if output.exists():
        old = json.loads(output.read_text())
        for key in ("manifest_sha256", "model", "effort", "template_sha256"):
            if old.get(key) != state[key]:
                raise ValueError(f"续跑不匹配（{key}）：换一个 --out 文件")
        state["results"] = old.get("results", {})

    def save():
        tmp = output.with_suffix(output.suffix + ".tmp")
        tmp.write_text(json.dumps(state, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        os.replace(tmp, output)

    install = str(uuid.uuid4())
    output.parent.mkdir(parents=True, exist_ok=True)
    for problem in problems:
        key = str(problem["index"])
        if key in state["results"] and state["results"][key].get("status") == "ok":
            continue
        try:
            result = send(d, model, problem["question"], args.effort, str(uuid.uuid4()), install)
        except (OSError, ValueError) as err:
            result = {"status": "failed", "error": str(err)[:300]}
        result.update(problem)
        if result["status"] == "ok":
            text = re.sub("[\u200b\u200c\u200d\u2060\ufeff,]", "", result["text"])
            numbers = re.findall(r"-?[0-9]+", text)
            result["correct"] = bool(numbers and int(numbers[-1]) == problem["answer"])
        state["results"][key] = result
        save()
        usage = result.get("usage") or {}
        text = result.get("text", "")
        print(key, result["kind"], result["status"], "推理", (usage.get("output_tokens_details") or {}).get("reasoning_tokens"),
              "正文", text[-40:], flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    a = sub.add_parser("anchor", help="标定完整伪装的官方基准输入 token")
    a.add_argument("disguise")
    a.add_argument("models", nargs="+")
    m = sub.add_parser("math", help="逐题发送导出清单里的推理题并落盘")
    m.add_argument("disguise")
    m.add_argument("model")
    m.add_argument("manifest")
    m.add_argument("--out")
    m.add_argument("--effort", default="medium")
    argv = sys.argv[1:]
    if argv and argv[0] not in ("anchor", "math", "-h", "--help"):
        argv = ["anchor", *argv]
    args = parser.parse_args(argv)
    if args.command == "anchor":
        anchor(args)
    else:
        math(args)


if __name__ == "__main__":
    main()
