#!/usr/bin/env python3
"""Ledger for sec-range v0 (sqlite; PG when the lane stands up)."""
import json
import os
import sqlite3
import time

SCHEMA = """
CREATE TABLE IF NOT EXISTS episodes(
  id INTEGER PRIMARY KEY, ts REAL, seed INTEGER, surface TEXT,
  persona TEXT, red_model TEXT, victim_model TEXT, outcome TEXT,
  verdict TEXT, techniques TEXT, severity TEXT);
CREATE TABLE IF NOT EXISTS attempts(
  id INTEGER PRIMARY KEY, episode_id INTEGER, idx INTEGER,
  action TEXT);
CREATE TABLE IF NOT EXISTS findings(
  id INTEGER PRIMARY KEY, episode_id INTEGER, what_happened TEXT,
  repro TEXT, defense_notes TEXT, ts REAL);
"""


class Ledger:
    def __init__(self, path):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        self.db = sqlite3.connect(path)
        self.db.executescript(SCHEMA)

    def episode(self, seed, cfg, verdict):
        cur = self.db.execute(
            "INSERT INTO episodes(ts, seed, surface, persona, red_model,"
            " victim_model, outcome, verdict, techniques, severity)"
            " VALUES(?,?,?,?,?,?,?,?,?,?)",
            (time.time(), seed, "S3-ingestion", "injector",
             cfg["models"]["red"], cfg["models"]["victim"],
             verdict.get("outcome"), json.dumps(verdict)[:4000],
             json.dumps(verdict.get("techniques", [])),
             verdict.get("severity")))
        self.db.commit()
        return cur.lastrowid

    def attempts(self, episode_id, actions):
        self.db.executemany(
            "INSERT INTO attempts(episode_id, idx, action) VALUES(?,?,?)",
            [(episode_id, i, a[:8000]) for i, a in enumerate(actions)])
        self.db.commit()

    def finding(self, episode_id, verdict):
        if verdict.get("outcome") in ("success", "blocked_but_leaky"):
            def _s(v):
                return v if isinstance(v, str) else json.dumps(v, default=str)
            self.db.execute(
                "INSERT INTO findings(episode_id, what_happened, repro,"
                " defense_notes, ts) VALUES(?,?,?,?,?)",
                (episode_id, _s(verdict.get("what_happened", "")),
                 _s(verdict.get("repro", "")), _s(verdict.get("defense_notes", "")),
                 time.time()))
            self.db.commit()

    def summary(self):
        rows = self.db.execute(
            "SELECT outcome, COUNT(*) FROM episodes GROUP BY outcome").fetchall()
        return dict(rows)
