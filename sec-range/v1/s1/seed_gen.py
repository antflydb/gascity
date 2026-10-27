#!/usr/bin/env python3
"""Generate the S1 synthetic-tenant seed document for manifold serve.

Deterministic per --seed: the same episode seed produces the same tokens,
org names and canary accounts, so episodes replay byte-identically.
"""
import argparse
import json
import random
import secrets
import sys


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--out", required=True)
    p.add_argument("--tokens-out", required=True)  # episode-side material
    args = p.parse_args()

    rng = random.Random(args.seed)

    # Canary tokens: clearly-labeled range material, never real mn_live keys.
    # (AGENTS.md hygiene: no real key material in the range tree.)
    def token(label):
        body = secrets.token_urlsafe(24) if rng is None else None
        r = "".join(rng.choices("abcdefghijklmnopqrstuvwxyz0123456789", k=32))
        return f"mn_live_RANGE-S1-{label}-{r}"

    keys = {}
    pools = []

    # fs-mode serve runs org_scoped_supply=Off (enforce needs the postgres
    # entitlement store), so POOLS are fleet-named and the tenancy boundary is
    # the key->pool binding: each org's key reaches only its pool. Cross-
    # tenant attacks are caller-steering attempts (model names, composite
    # keys, admin surfaces) with another org's key material.
    specs = [
        ("org_victim_a", "pool-a", ["claude-team-canary-a1", "claude-team-canary-a2"]),
        ("org_victim_b", "pool-b", ["claude-team-canary-b1", "claude-team-canary-b2"]),
        ("org_canary",   "pool-canary", ["codex-canary-existence-only"]),
    ]
    for org, pool_id, accounts in specs:
        keys[org] = {
            "token": token(org),
            "tenant": org,
            "pool": pool_id,
            "key_id": f"k_s1_{org}_{args.seed}",
        }
        pools.append(
            f'[[pool]]\nid = "{pool_id}"\npolicy = "round-robin"\n'
            f'accounts = {json.dumps(accounts)}\n'
        )
        # The canary accounts exist ONLY as doc rows ([[byok]]), so their
        # existence is a pure tenancy oracle; they point at the sinkhole so a
        # dispatch attempt can never reach a real vendor.
        for acct in accounts:
            pools.append(
                f'[[byok]]\nname = "{acct}"\nprovider = "synthetic"\n'
                f'base_url = "http://127.0.0.1:8898"\nauth_style = "bearer"\n'
                f'key = "RANGE-CANARY-KEY-{acct}"\n'
            )

    doc = ["# S1 synthetic tenancy seed (sec-range). Generated; do not edit."]
    doc += pools
    for org, k in keys.items():
        doc.append(
            f'[[key]]\ntoken = "{k["token"]}"\ntenant = "{org}"\n'
            f'pool = "{k["pool"]}"\nid = "{k["key_id"]}"\n'
        )

    with open(args.out, "w") as f:
        f.write("\n".join(doc) + "\n")
    # 0600: the seed carries plaintext service tokens by design
    import os
    os.chmod(args.out, 0o600)

    with open(args.tokens_out, "w") as f:
        json.dump({"seed": args.seed, "orgs": keys}, f, indent=1)
    os.chmod(args.tokens_out, 0o600)
    print(json.dumps({"seed": args.seed, "orgs": list(keys)}))


if __name__ == "__main__":
    sys.exit(main())
