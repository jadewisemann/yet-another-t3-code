#!/usr/bin/env python3
"""A stand-in T3 Code server for trying the UI without a real one.

It speaks just enough of the orchestration RPC protocol: a shell with one project and three
threads, thread snapshots, renames, and a canned streaming reply to any message.

    pip install websockets==10.4
    python3 scripts/fake-server.py            # listens on ws://127.0.0.1:3999
    T3_SERVER_URL=http://127.0.0.1:3999 T3_TOKEN=x cargo run

Every frame the client sends is written to fake-server.log.
"""
import asyncio, json, sys, websockets
LOG = open(sys.argv[1] if len(sys.argv) > 1 else "fake-server.log", "w")
NOW = "2026-11-24T08:00:00.000Z"
def thread(tid, title, status="idle", t="2026-11-24T07:00:00.000Z"):
    return {"id": tid, "projectId": "p1", "title": title, "status": status,
            "modelSelection": {"instanceId": "claude", "model": "claude-opus"},
            "runtimeMode": "full-access", "interactionMode": "default",
            "lineage": {"parentThreadId": None, "relationshipToParent": None, "rootThreadId": tid},
            "archivedAt": None, "deletedAt": None, "updatedAt": t, "latestUserMessageAt": t,
            "pendingRuntimeRequest": None}
THREADS = {"t1": thread("t1", "Fix Safari export button", "running", "2026-11-24T07:30:00.000Z"),
           "t2": thread("t2", "Add dark mode toggle"),
           "t3": thread("t3", "Investigate flaky CI", t="2026-11-23T07:00:00.000Z")}
MESSAGES = {"t1": [
  {"id": "m1", "role": "user", "text": "The export button does nothing on Safari. Can you look?", "streaming": False, "createdAt": "2026-11-24T07:30:00.000Z"},
  {"id": "m2", "role": "assistant", "text": "I found it: the click handler uses `showSaveFilePicker`, which Safari does not support.\nI'll add a fallback that creates a download link instead.", "streaming": False, "createdAt": "2026-11-24T07:30:05.000Z"}]}
async def handler(ws, path):
    auth = ws.request_headers.get("authorization")
    LOG.write(json.dumps({"path": path, "auth": auth}) + "\n"); LOG.flush()
    shell_id = None
    thread_ids = {}
    async for raw in ws:
        LOG.write(raw + "\n"); LOG.flush()
        msg = json.loads(raw)
        if msg["_tag"] != "Request": continue
        rid, tag, payload = msg["id"], msg["tag"], msg["payload"]
        if tag == "orchestration.subscribeShell":
            shell_id = rid
            snap = {"schemaVersion": 1, "snapshotSequence": 10, "threads": list(THREADS.values()), "archivedThreads": [],
                    "projects": [{"id": "p1", "title": "yet-another-t3-code", "workspaceRoot": "/w", "defaultModelSelection": None, "scripts": [], "createdAt": NOW, "updatedAt": NOW}]}
            await ws.send(json.dumps({"_tag": "Chunk", "requestId": rid, "values": [{"kind": "snapshot", "snapshot": snap}, {"kind": "synchronized"}]}))
        elif tag == "orchestration.subscribeThread":
            tid = payload["threadId"]
            thread_ids[tid] = rid
            await ws.send(json.dumps({"_tag": "Chunk", "requestId": rid, "values": [{"kind": "snapshot", "snapshotSequence": 3, "projection": {"messages": MESSAGES.get(tid, [])}}, {"kind": "synchronized"}]}))
        elif tag == "orchestration.dispatchCommand":
            await ws.send(json.dumps({"_tag": "Exit", "requestId": rid, "exit": {"_tag": "Success", "value": {"sequence": 11}}}))
            if payload["type"] == "message.dispatch" and payload["threadId"] in thread_ids:
                tr = thread_ids[payload["threadId"]]
                user = {"id": payload["messageId"], "role": "user", "text": payload["text"], "streaming": False, "createdAt": "2026-11-24T08:10:00.000Z"}
                await ws.send(json.dumps({"_tag": "Chunk", "requestId": tr, "values": [{"kind": "event", "sequence": 20, "event": {"type": "message.updated", "payload": user}}]}))
                for i, part in enumerate(["Sure.", "Sure. Here is the plan:", "Sure. Here is the plan:\n1. Detect support\n2. Fall back to an <a download> link"]):
                    await asyncio.sleep(0.3)
                    a = {"id": "reply-" + payload["messageId"], "role": "assistant", "text": part, "streaming": i < 2, "createdAt": "2026-11-24T08:10:01.000Z"}
                    await ws.send(json.dumps({"_tag": "Chunk", "requestId": tr, "values": [{"kind": "event", "sequence": 21 + i, "event": {"type": "message.updated", "payload": a}}]}))
            if payload["type"] == "thread.metadata.update":
                THREADS[payload["threadId"]]["title"] = payload["title"]
                await ws.send(json.dumps({"_tag": "Chunk", "requestId": shell_id, "values": [{"kind": "thread.updated", "sequence": 12, "location": "active", "thread": THREADS[payload["threadId"]]}]}))
        else:
            await ws.send(json.dumps({"_tag": "Exit", "requestId": rid, "exit": {"_tag": "Failure", "cause": [{"_tag": "Fail", "error": {"_tag": "X", "message": "unsupported in fake"}}]}}))
async def main():
    async with websockets.serve(handler, "127.0.0.1", 3999):
        await asyncio.Future()
asyncio.run(main())
