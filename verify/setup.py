#!/usr/bin/env python3
"""
ISSUE-001 setup: initialize AxonHub, create two channels pointing at the mock
upstream, create an API key, and pin the key's active profile to ONE channel
(channel A) so assumption #1 (single-channel isolation) can be tested.

Pure stdlib. Run AFTER `docker compose -f docker-compose.verify.yml up -d`.
Endpoints/inputs are taken from AxonHub commit ed6119a1 GraphQL schema.
If a call errors, the script prints the raw response — paste it back to iterate.

Outputs verify/state.json with the api key + channel ids for run_tests.sh.
"""
import json
import time
import urllib.request
import urllib.error

BASE = "http://localhost:8090"
OWNER = {"ownerEmail": "verify@example.com", "ownerPassword": "verify123",
         "ownerFirstName": "Ver", "ownerLastName": "Ify", "brandName": "VerifyLab",
         "preferLanguage": "en"}


def call(path, body, token=None, method="POST"):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode() or "{}")


def gql(query, variables, token):
    st, resp = call("/admin/graphql", {"query": query, "variables": variables}, token)
    if "errors" in resp:
        print("GraphQL error:", json.dumps(resp["errors"], ensure_ascii=False))
    return resp.get("data") or {}


def wait_health():
    for _ in range(60):
        try:
            with urllib.request.urlopen(BASE + "/health", timeout=3) as r:
                if r.status == 200:
                    return True
        except Exception:
            time.sleep(2)
    return False


CREATE_CHANNEL = """
mutation($input: CreateChannelInput!) {
  createChannel(input: $input) { id name }
}"""

CREATE_APIKEY = """
mutation($input: CreateAPIKeyInput!) {
  createAPIKey(input: $input) { id key }
}"""

UPDATE_PROFILES = """
mutation($id: ID!, $input: UpdateAPIKeyProfilesInput!) {
  updateAPIKeyProfiles(id: $id, input: $input) { id profiles { activeProfile } }
}"""

ENABLE_CHANNEL = """
mutation($id: ID!, $status: ChannelStatus!) {
  updateChannelStatus(id: $id, status: $status) { id name status }
}"""

# Configure per-model prices so usage_log gets total_cost + costPriceReferenceID
# (assumption #4 cost path). usagePerUnit is price per 1,000,000 tokens.
SAVE_PRICES = """
mutation($cid: ID!, $input: [SaveChannelModelPriceInput!]!) {
  saveChannelModelPrices(channelId: $cid, input: $input) { modelID }
}"""


def _price_item(code, per_million):
    return {"itemCode": code,
            "pricing": {"mode": "usage_per_unit", "usagePerUnit": str(per_million)}}


def channel_input(name):
    return {
        "type": "openai",
        "baseURL": "http://mock-upstream:8091",
        "name": name,
        "credentials": {"apiKey": "mock-key"},
        "supportedModels": ["mock-normal", "mock-normal-2", "mock-empty-sse",
                             "mock-slow-first", "mock-heartbeat", "mock-500",
                             "mock-abort"],
        "defaultTestModel": "mock-normal",
    }


def main():
    print("waiting for AxonHub health...")
    if not wait_health():
        print("AxonHub did not become healthy on :8090"); return
    print("initializing system...")
    st, resp = call("/admin/system/initialize", OWNER)
    print("  init:", st, resp.get("message", resp))
    st, resp = call("/admin/auth/signin", {"email": OWNER["ownerEmail"], "password": OWNER["ownerPassword"]})
    token = resp.get("token") or (resp.get("data") or {}).get("token")
    if not token:
        print("  signin failed:", st, resp); return
    print("  signed in.")

    a = gql(CREATE_CHANNEL, {"input": channel_input("chan-A-mock")}, token)
    b = gql(CREATE_CHANNEL, {"input": channel_input("chan-B-mock")}, token)
    ca = (a.get("createChannel") or {}).get("id")
    cb = (b.get("createChannel") or {}).get("id")
    print("  channel A:", ca, " channel B:", cb)
    if not ca:
        print("  channel creation failed — see error above"); return

    # channels are created 'disabled'; enable both so models become routable
    for cid in (ca, cb):
        if cid:
            e = gql(ENABLE_CHANNEL, {"id": cid, "status": "enabled"}, token)
            print("  enabled:", json.dumps(e.get("updateChannelStatus") or {}, ensure_ascii=False))

    # price mock-normal / mock-heartbeat on channel A so usage_log gets cost
    # (prices per 1M tokens: prompt=1, completion=2, cached=0.5)
    price_input = [
        {"modelId": "mock-normal", "price": {"items": [
            _price_item("prompt_tokens", 1), _price_item("completion_tokens", 2),
            _price_item("prompt_cached_tokens", "0.5")]}},
        {"modelId": "mock-heartbeat", "price": {"items": [
            _price_item("prompt_tokens", 1), _price_item("completion_tokens", 2)]}},
    ]
    p = gql(SAVE_PRICES, {"cid": ca, "input": price_input}, token)
    print("  prices set:", json.dumps([m.get("modelID") for m in
          (p.get("saveChannelModelPrices") or [])], ensure_ascii=False))

    k = gql(CREATE_APIKEY, {"input": {"name": "verify-key", "type": "user",
            "projectID": "gid://axonhub/Project/1"}}, token)
    key_obj = k.get("createAPIKey") or {}
    key_id, key_val = key_obj.get("id"), key_obj.get("key")
    print("  api key id:", key_id)
    if not key_id:
        print("  api key creation failed — see error above"); return

    # pin active profile to channel A only (single-channel isolation)
    # channelIDs is [Int]; channel gid is like gid://axonhub/Channel/1 -> 1
    ca_num = int(str(ca).rsplit("/", 1)[-1])
    prof = gql(UPDATE_PROFILES, {"id": key_id, "input": {
        "activeProfile": "isolated",
        "profiles": [{"name": "isolated", "channelIDs": [ca_num]}],
    }}, token)
    print("  profile pinned:", json.dumps(prof, ensure_ascii=False))

    # warmup: model routing becomes ready a few seconds after channel enable
    # (async model sync). Poll a test chat until mock-normal resolves so that
    # run_tests.sh does not race into "model not found".
    print("  warming up model routing...")
    for i in range(30):
        st, resp = call("/v1/chat/completions",
                        {"model": "mock-normal", "stream": False,
                         "messages": [{"role": "user", "content": "ping"}]},
                        token=key_val)
        err = (resp.get("error") or {}).get("message", "") if isinstance(resp, dict) else ""
        if "model not found" not in err:
            print(f"  routing ready after {i}x (status {st})"); break
        time.sleep(1)
    else:
        print("  WARNING: model still not routable after warmup; run_tests may race")

    state = {"token": token, "apiKey": key_val, "keyId": key_id,
             "channelA": ca, "channelB": cb}
    with open("state.json", "w") as f:
        json.dump(state, f, indent=2)
    print("\nSaved state.json. API key:", key_val)
    print("Next: ./run_tests.sh")


if __name__ == "__main__":
    main()
