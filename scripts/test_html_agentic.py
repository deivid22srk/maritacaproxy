#!/usr/bin/env python3
"""
Test agentic tool-call loop with MaritacaProxy.
Asks the model to create and edit HTML files using tool calls.
"""
import json
import os
import sys
import time
import requests

PROXY_URL = "http://localhost:3000/v1"
MODEL = "sabia-4"
WORK_DIR = "/tmp/maritaca_html_test"

os.makedirs(WORK_DIR, exist_ok=True)

# ─── Tool definitions ──────────────────────────────────────────────────────

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "create_html_file",
            "description": "Create a new HTML file with the given content. Overwrites if file exists.",
            "parameters": {
                "type": "object",
                "properties": {
                    "filename": {"type": "string", "description": "Name of the HTML file (e.g. 'index.html')"},
                    "content": {"type": "string", "description": "Full HTML content of the file"},
                },
                "required": ["filename", "content"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "edit_html_file",
            "description": "Edit an existing HTML file by replacing old_text with new_text.",
            "parameters": {
                "type": "object",
                "properties": {
                    "filename": {"type": "string", "description": "Name of the file to edit"},
                    "old_text": {"type": "string", "description": "Exact text to find in the file"},
                    "new_text": {"type": "string", "description": "Text to replace old_text with"},
                },
                "required": ["filename", "old_text", "new_text"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "read_html_file",
            "description": "Read the content of an HTML file.",
            "parameters": {
                "type": "object",
                "properties": {
                    "filename": {"type": "string", "description": "Name of the file to read"},
                },
                "required": ["filename"],
            },
        },
    },
]

# ─── Tool executors (run locally) ──────────────────────────────────────────

def execute_tool(name, args):
    print(f"  -> Executing {name}({json.dumps(args, ensure_ascii=False)[:200]})")
    try:
        if name == "create_html_file":
            path = os.path.join(WORK_DIR, args["filename"])
            with open(path, "w", encoding="utf-8") as f:
                f.write(args["content"])
            return f"File created: {path} ({len(args['content'])} bytes)"

        elif name == "edit_html_file":
            path = os.path.join(WORK_DIR, args["filename"])
            if not os.path.exists(path):
                return f"ERROR: File {args['filename']} not found"
            with open(path, "r", encoding="utf-8") as f:
                content = f.read()
            if args["old_text"] not in content:
                return f"ERROR: old_text not found in {args['filename']}"
            new_content = content.replace(args["old_text"], args["new_text"], 1)
            with open(path, "w", encoding="utf-8") as f:
                f.write(new_content)
            return f"File edited: {args['filename']} (replaced {len(args['old_text'])} chars with {len(args['new_text'])} chars)"

        elif name == "read_html_file":
            path = os.path.join(WORK_DIR, args["filename"])
            if not os.path.exists(path):
                return f"ERROR: File {args['filename']} not found"
            with open(path, "r", encoding="utf-8") as f:
                return f.read()

        else:
            return f"ERROR: Unknown tool '{name}'"
    except Exception as e:
        return f"ERROR: {type(e).__name__}: {e}"

# ─── Chat client ───────────────────────────────────────────────────────────

def chat_completion(messages, tools=None):
    payload = {
        "model": MODEL,
        "messages": messages,
        "stream": False,
    }
    if tools:
        payload["tools"] = tools

    resp = requests.post(
        f"{PROXY_URL}/chat/completions",
        json=payload,
        headers={"Content-Type": "application/json"},
        timeout=180,
    )
    resp.raise_for_status()
    return resp.json()

# ─── Main agentic loop ────────────────────────────────────────────────────

def main():
    print(f"=== MaritacaProxy Agentic HTML Test ===")
    print(f"Model: {MODEL}")
    print(f"Work dir: {WORK_DIR}")
    print()

    # Initial prompt
    messages = [
        {
            "role": "system",
            "content": (
                "You are a web developer assistant. Use the provided tools to create and edit HTML files. "
                "Always use the tools - do NOT output HTML directly in your text response. "
                "After completing the task, summarize what you did in a short text response."
            ),
        },
        {
            "role": "user",
            "content": (
                "Create a simple HTML landing page for a coffee shop called 'Cafe do Vinte'. "
                "It should have: a hero section with the shop name and a tagline, "
                "a menu section with 3 coffee items (Espresso, Cappuccino, Latte) and their prices, "
                "and a footer with contact info. Use inline CSS for styling. "
                "Save it as index.html. After creating, read it back to verify, "
                "then add a 4th menu item 'Mocha' priced at R$ 8,50 using the edit tool."
            ),
        },
    ]

    max_turns = 10
    for turn in range(1, max_turns + 1):
        print(f"\n--- Turn {turn} ---")
        t0 = time.time()

        try:
            result = chat_completion(messages, tools=TOOLS)
        except Exception as e:
            print(f"API error: {e}")
            return

        elapsed = time.time() - t0
        choice = result["choices"][0]
        msg = choice["message"]
        finish = choice["finish_reason"]

        print(f"  {elapsed:.1f}s | finish_reason: {finish}")

        # Show content if any
        if msg.get("content"):
            content = msg["content"]
            print(f"  Content ({len(content)} chars):")
            print(f"    {content[:500]}")
            if len(content) > 500:
                print(f"    ... ({len(content)} total)")

        # Show reasoning if any
        if msg.get("reasoning_content"):
            preview = msg["reasoning_content"][:200]
            print(f"  Reasoning: {preview}...")

        # Process tool calls
        tool_calls = msg.get("tool_calls") or []
        if not tool_calls:
            print(f"\n=== No more tool calls - task complete! ===")
            break

        # Add assistant message to history
        messages.append({
            "role": "assistant",
            "content": msg.get("content"),
            "tool_calls": tool_calls,
        })

        # Execute each tool call and add results
        for tc in tool_calls:
            fn = tc["function"]
            name = fn["name"]
            try:
                args = json.loads(fn["arguments"])
            except json.JSONDecodeError as e:
                args = {}
                print(f"  Failed to parse arguments: {e}")

            tool_result = execute_tool(name, args)
            preview = tool_result[:200] if len(tool_result) > 200 else tool_result
            print(f"  Result: {preview}{'...' if len(tool_result)>200 else ''}")

            messages.append({
                "role": "tool",
                "tool_call_id": tc["id"],
                "name": name,
                "content": tool_result,
            })

    # Show final files
    print(f"\n=== Files in {WORK_DIR} ===")
    for fname in sorted(os.listdir(WORK_DIR)):
        path = os.path.join(WORK_DIR, fname)
        size = os.path.getsize(path)
        print(f"  {fname} ({size} bytes)")

    # Show final index.html
    index_path = os.path.join(WORK_DIR, "index.html")
    if os.path.exists(index_path):
        print(f"\n=== Final index.html (first 1500 chars) ===")
        with open(index_path, "r") as f:
            content = f.read()
        print(content[:1500])
        if len(content) > 1500:
            print(f"... ({len(content)} total bytes)")

if __name__ == "__main__":
    main()
