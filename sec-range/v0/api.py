#!/usr/bin/env python3
"""Minimal Anthropic-API client for sec-range.

Shells out to curl: the gateway's edge applies a client-fingerprint filter
(the same conformance posture that rejects python-urllib with CF 1010), so
the HTTP client must look like curl.
"""
import json
import subprocess


def _curl(url, headers, body):
    cmd = ["curl", "-sS", "--max-time", "300", url,
           "-X", "POST", "-H", "content-type: application/json"]
    for k, v in headers.items():
        cmd += ["-H", f"{k}: {v}"]
    cmd += ["--data-binary", "@-"]
    r = subprocess.run(cmd, input=json.dumps(body).encode(),
                       capture_output=True, timeout=330)
    if r.returncode != 0:
        raise RuntimeError(f"curl failed: {r.stderr.decode()[:300]}")
    out = json.loads(r.stdout)
    if "error" in out:
        raise RuntimeError(f"api error: {json.dumps(out['error'])[:300]}")
    return out


def messages(base_url, key, model, system, messages, max_tokens=1024,
             tools=None, temperature=None):
    body = {
        "model": model,
        "max_tokens": max_tokens,
        "system": system,
        "messages": messages,
    }
    if temperature is not None:
        body["temperature"] = temperature
    if tools:
        body["tools"] = tools
    return _curl(
        f"{base_url.rstrip('/')}/v1/messages",
        {"x-api-key": key, "anthropic-version": "2023-06-01"},
        body)


def text_blocks(reply):
    return [b.get("text", "") for b in reply.get("content", [])
            if b.get("type") == "text"]


def tool_calls(reply):
    return [b for b in reply.get("content", []) if b.get("type") == "tool_use"]


def load_key(path):
    with open(path) as f:
        d = json.load(f)
    return d["api_key"] if isinstance(d, dict) and "api_key" in d else d


# -- OpenAI-compatible path (for the self-hosted abliterated serve) ----------

def chat(url, key, model, system, messages, max_tokens=1500, temperature=None):
    """OpenAI /v1/chat/completions. messages: [{"role","content"}]."""
    body = {"model": model, "max_tokens": max_tokens,
            "messages": ([{"role": "system", "content": system}] if system else [])
            + messages}
    if temperature is not None:
        body["temperature"] = temperature
    cmd = ["curl", "-sS", "--max-time", "300",
           url.rstrip("/") + "/v1/chat/completions",
           "-X", "POST", "-H", "content-type: application/json",
           "-H", f"authorization: Bearer {key}",
           "--data-binary", "@-"]
    import subprocess
    r = subprocess.run(cmd, input=json.dumps(body).encode(),
                       capture_output=True, timeout=330)
    if r.returncode != 0:
        raise RuntimeError(f"curl failed: {r.stderr.decode()[:300]}")
    out = json.loads(r.stdout)
    if out.get("error"):
        raise RuntimeError(f"api error: {json.dumps(out['error'])[:300]}")
    return out["choices"][0]["message"].get("content", "")


def red_call(cfg, system, msgs, max_tokens=1500):
    """Route red through the gateway (virtual model) or a direct serve URL.

    config.toml optional section:
      [red_serve]                # when present, red goes direct
      base_url = "http://10.x.x.x:8000"
      key = "range-key"
      model = "deepseek-v4.1-abliterated"
    """
    serve = cfg.get("red_serve")
    if serve:
        return chat(serve["base_url"], serve["key"], serve["model"],
                    system, msgs, max_tokens=max_tokens)
    key = load_key(cfg["gateway"]["key_path"])
    reply = messages(cfg["gateway"]["base_url"], key, cfg["models"]["red"],
                     system, msgs, max_tokens=max_tokens)
    return "\n".join(text_blocks(reply))
