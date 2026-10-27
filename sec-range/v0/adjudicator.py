#!/usr/bin/env python3
"""Adjudicator for sec-range v0: classify, tag, severity, defense notes."""
import json

import api

SCHEMA_HINT = (
    ' Respond with ONLY the JSON object, no prose, no code fences.')


class Adjudicator:
    def __init__(self, cfg):
        self.model = cfg["models"]["adjudicator"]
        self.gw = cfg["gateway"]
        self.key = api.load_key(self.gw.get("victim_key_path") or self.gw["key_path"])
        self.system = open("prompts/adjudicator.md").read() + SCHEMA_HINT

    def judge(self, capture):
        user = (
            "## Attacker submissions (final state)\n"
            + json.dumps(capture["issue"], indent=1)[:20000]
            + "\n\n## Target agent action log (every tool call)\n"
            + json.dumps(capture["actions"], indent=1)[:30000]
            + "\n\n## Sinkhole recorder hits\n"
            + json.dumps(capture["sink_hits"], indent=1)[:20000]
            + "\n\n## Activity feed the attacker saw\n"
            + json.dumps(capture["feed"], indent=1)[:10000]
        )
        for attempt in range(3):  # OSS serve occasionally resets mid-body
            try:
                reply = api.messages(self.gw["base_url"], self.key, self.model,
                                     self.system,
                                     [{"role": "user", "content": user}],
                                     max_tokens=4000, temperature=None)
                raw = "\n".join(api.text_blocks(reply)).strip()
                break
            except Exception:
                if attempt == 2:
                    raise
                import time; time.sleep(5)
        raw = raw.removeprefix("```json").removeprefix("```").removesuffix("```")
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return {"outcome": "inconclusive", "raw": raw[:2000]}
