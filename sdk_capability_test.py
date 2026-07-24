import argparse
import base64
import json
import os
import re
import struct
import time
from pathlib import Path

import anthropic
import openai
from google import genai
from google.genai import types


BASE_URL = "http://127.0.0.1:8080"
TOKEN = os.environ["ZENCODER2API_TOKEN"]
IMAGE_PATH = Path(os.environ["ZENCODER2API_TEST_IMAGE"])
RESULTS_PATH = Path(os.environ.get("ZENCODER2API_RESULTS_PATH", "SDK_CAPABILITY_RESULTS.jsonl"))
GENERATED_DIR = Path("sdk_generated_images")

ANTHROPIC_MODELS = [
    "claude-haiku-4-5",
    "claude-haiku-4-5-20251001",
    "claude-sonnet-4-6",
    "claude-sonnet-5",
    "claude-opus-4-6",
    "claude-opus-4-7",
    "claude-opus-4-8",
    "minimax-m3",
    "glm-5.2",
]
GEMINI_MODELS = [
    "gemini-3.1-pro-preview",
    "gemini-3.1-pro-preview-customtools",
    "gemini-3-flash-preview",
    "gemini-3.5-flash",
    "gemini-3.1-flash-image",
]
OPENAI_MODELS = [
    "gpt-5-nano",
    "gpt-5.1-codex-mini",
    "gpt-5.1-codex-max",
    "gpt-5.3-codex",
    "gpt-5.4-mini",
    "gpt-5.4",
    "gpt-5.5",
    "gpt-5.6-luna",
    "gpt-5.6-terra",
    "grok-code-fast",
    "grok-4.5",
]
CODEX_MODELS = {"gpt-5.1-codex-mini", "gpt-5.1-codex-max", "gpt-5.3-codex"}

SCHEMA = {
    "type": "object",
    "properties": {
        "city": {"type": "string"},
        "temperature_c": {"type": "integer"},
        "rain": {"type": "boolean"},
    },
    "required": ["city", "temperature_c", "rain"],
    "additionalProperties": False,
}
GEMINI_SCHEMA = {key: value for key, value in SCHEMA.items() if key != "additionalProperties"}
TOOL_SCHEMA = {
    "type": "object",
    "properties": {"city": {"type": "string"}},
    "required": ["city"],
}


openai_client = openai.OpenAI(api_key=TOKEN, base_url=BASE_URL + "/v1", timeout=90, max_retries=0)
anthropic_client = anthropic.Anthropic(api_key=TOKEN, base_url=BASE_URL, timeout=90, max_retries=0)
gemini_client = genai.Client(
    api_key=TOKEN,
    http_options=types.HttpOptions(base_url=BASE_URL, api_version="v1beta", timeout=90_000),
)


def append_result(model, sdk, capability, started, status="pass", **details):
    record = {
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "model": model,
        "sdk": sdk,
        "capability": capability,
        "status": status,
        "latency_seconds": round(time.perf_counter() - started, 3),
        **details,
    }
    with RESULTS_PATH.open("a", encoding="utf-8") as file:
        file.write(json.dumps(record, ensure_ascii=False) + "\n")
    print(json.dumps(record, ensure_ascii=False), flush=True)


def classify_error(exc):
    status = getattr(exc, "status_code", None) or getattr(exc, "code", None)
    if status in (400, 404, 422):
        category = "unsupported_or_request"
    elif status in (401, 403):
        category = "auth"
    elif status == 429:
        category = "rate_limit"
    elif isinstance(status, int) and status >= 500:
        category = "upstream"
    elif isinstance(exc, (TypeError, ValueError)):
        category = "client_validation"
    else:
        category = "runtime"
    return {
        "error_category": category,
        "http_status": status,
        "error_type": type(exc).__name__,
        "error": str(exc).replace(TOKEN, "<redacted>")[:1200],
    }


def run_case(model, sdk, capability, call):
    started = time.perf_counter()
    try:
        details = call()
        append_result(model, sdk, capability, started, status="pass" if details.get("ok") else "fail", **details)
    except Exception as exc:
        append_result(model, sdk, capability, started, status="fail", **classify_error(exc))


def valid_weather(value):
    return (
        isinstance(value, dict)
        and isinstance(value.get("city"), str)
        and isinstance(value.get("temperature_c"), int)
        and isinstance(value.get("rain"), bool)
    )


def answer_is_432(text):
    return bool(re.search(r"(?<!\d)432(?!\d)", text.replace(",", "")))


def summarize_usage(usage):
    if usage is None:
        return None
    if hasattr(usage, "model_dump"):
        return usage.model_dump(exclude_none=True)
    if isinstance(usage, dict):
        return usage
    return str(usage)


def anthropic_text(model):
    response = anthropic_client.messages.create(
        model=model,
        max_tokens=64,
        thinking={"type": "disabled"},
        messages=[
            {"role": "user", "content": "Remember the codeword ORBIT-73. Reply READY."},
            {"role": "assistant", "content": "READY"},
            {"role": "user", "content": "What codeword did I ask you to remember? Reply with only it."},
        ],
    )
    text = "".join(block.text for block in response.content if block.type == "text").strip()
    return {"ok": text.upper() == "ORBIT-73", "output": text[:240], "finish_reason": response.stop_reason, "usage": summarize_usage(response.usage)}


def anthropic_stream(model):
    with anthropic_client.messages.stream(
        model=model,
        max_tokens=64,
        thinking={"type": "disabled"},
        messages=[{"role": "user", "content": "Reply with exactly STREAM-OK."}],
    ) as stream:
        text = "".join(stream.text_stream).strip()
        final = stream.get_final_message()
    return {"ok": bool(text) and final.stop_reason is not None, "output": text[:240], "finish_reason": final.stop_reason, "usage": summarize_usage(final.usage)}


def anthropic_tool(model):
    response = anthropic_client.messages.create(
        model=model,
        max_tokens=128,
        thinking={"type": "disabled"},
        messages=[{"role": "user", "content": "Use lookup_weather for Paris, then report the returned temperature."}],
        tools=[{"name": "lookup_weather", "description": "Look up current weather", "input_schema": TOOL_SCHEMA}],
        tool_choice={"type": "tool", "name": "lookup_weather"},
    )
    calls = [block for block in response.content if block.type == "tool_use"]
    if not calls:
        return {"ok": False, "phase": "tool_call", "output": "no tool_use"}
    call = calls[0]
    followup = anthropic_client.messages.create(
        model=model,
        max_tokens=96,
        thinking={"type": "disabled"},
        messages=[
            {"role": "user", "content": "Use lookup_weather for Paris, then report the returned temperature."},
            {"role": "assistant", "content": [block.model_dump(exclude_none=True) for block in response.content]},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": call.id, "content": '{"city":"Paris","temperature_c":21,"rain":false}'}]},
        ],
        tools=[{"name": "lookup_weather", "description": "Look up current weather", "input_schema": TOOL_SCHEMA}],
    )
    text = "".join(block.text for block in followup.content if block.type == "text").strip()
    return {"ok": call.name == "lookup_weather" and call.input.get("city", "").lower() == "paris" and "21" in text, "tool_name": call.name, "tool_input": call.input, "final_output": text[:240]}


def anthropic_vision(model):
    data = base64.b64encode(IMAGE_PATH.read_bytes()).decode()
    response = anthropic_client.messages.create(
        model=model,
        max_tokens=96,
        thinking={"type": "disabled"},
        messages=[{"role": "user", "content": [
            {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": data}},
            {"type": "text", "text": "Describe the main vehicle and setting in this image. Do not guess from a filename."},
        ]}],
    )
    text = "".join(block.text for block in response.content if block.type == "text").strip()
    lower = text.lower()
    return {"ok": bool(text) and ("bus" in lower or "公交" in text or "巴士" in text), "output": text[:500]}


def anthropic_structured(model):
    response = anthropic_client.messages.create(
        model=model,
        max_tokens=128,
        thinking={"type": "disabled"},
        messages=[{"role": "user", "content": "Return Paris weather: 21 C and no rain."}],
        output_config={"format": {"type": "json_schema", "schema": SCHEMA}},
    )
    text = "".join(block.text for block in response.content if block.type == "text").strip()
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError as exc:
        return {"ok": False, "error_category": "response_validation", "validation_error": str(exc), "output": text[:500]}
    return {"ok": valid_weather(parsed), "output": parsed}


def anthropic_reasoning(model, effort):
    if "haiku" in model:
        thinking = {"type": "disabled"} if effort == "low" else {"type": "enabled", "budget_tokens": 4096}
        output_config = None
    elif model in ("minimax-m3", "glm-5.2"):
        budget = 1024 if effort == "low" else 4096
        thinking = {"type": "enabled", "budget_tokens": budget}
        output_config = None
    else:
        thinking = {"type": "adaptive"}
        output_config = {"effort": effort}
    kwargs = {
        "model": model,
        "max_tokens": 4352 if ("haiku" in model and effort == "high") or (model in ("minimax-m3", "glm-5.2") and effort == "high") else 512 if "haiku" in model else 1280 if model in ("minimax-m3", "glm-5.2") else 512,
        "thinking": thinking,
        "messages": [{"role": "user", "content": "Find the smallest positive integer divisible by both 18 and 24 that leaves remainder 5 when divided by 7. Reply with only the integer."}],
    }
    if output_config:
        kwargs["output_config"] = output_config
    response = anthropic_client.messages.create(**kwargs)
    thoughts = [block for block in response.content if block.type in ("thinking", "redacted_thinking")]
    text = "".join(block.text for block in response.content if block.type == "text").strip()
    return {"ok": answer_is_432(text), "format_exact": text.replace(",", "").strip() == "432", "effort": effort, "output": text[:240], "thinking_blocks": len(thoughts), "thinking_chars": sum(len(getattr(block, "thinking", "") or "") for block in thoughts), "usage": summarize_usage(response.usage)}


def openai_text(model):
    if model in CODEX_MODELS:
        response = openai_client.responses.create(
            model=model,
            max_output_tokens=1024,
            input=[
                {"role": "user", "content": "Remember the codeword ORBIT-73. Reply READY."},
                {"role": "assistant", "content": "READY"},
                {"role": "user", "content": "What codeword did I ask you to remember? Reply with only it."},
            ],
        )
        text = response.output_text.strip()
        return {"ok": text.upper() == "ORBIT-73", "output": text[:240], "finish_reason": response.status, "usage": summarize_usage(response.usage)}
    response = openai_client.chat.completions.create(
        model=model,
        max_completion_tokens=96,
        messages=[
            {"role": "user", "content": "Remember the codeword ORBIT-73. Reply READY."},
            {"role": "assistant", "content": "READY"},
            {"role": "user", "content": "What codeword did I ask you to remember? Reply with only it."},
        ],
    )
    text = (response.choices[0].message.content or "").strip()
    return {"ok": text.upper() == "ORBIT-73", "output": text[:240], "finish_reason": response.choices[0].finish_reason, "usage": summarize_usage(response.usage)}


def openai_stream(model):
    if model in CODEX_MODELS:
        text_parts = []
        events = 0
        completed = False
        for event in openai_client.responses.create(
            model=model,
            max_output_tokens=1024,
            stream=True,
            input="Reply with exactly STREAM-OK.",
        ):
            events += 1
            if event.type == "response.output_text.delta":
                text_parts.append(event.delta)
            elif event.type == "response.completed":
                completed = True
        text = "".join(text_parts).strip()
        return {"ok": bool(text) and completed, "output": text[:240], "finish_reason": "completed" if completed else None, "events": events}
    text_parts = []
    finish = None
    events = 0
    for chunk in openai_client.chat.completions.create(
        model=model,
        max_completion_tokens=96,
        stream=True,
        messages=[{"role": "user", "content": "Reply with exactly STREAM-OK."}],
    ):
        events += 1
        for choice in chunk.choices:
            if choice.delta.content:
                text_parts.append(choice.delta.content)
            if choice.finish_reason:
                finish = choice.finish_reason
    text = "".join(text_parts).strip()
    return {"ok": bool(text) and finish is not None, "output": text[:240], "finish_reason": finish, "events": events}


def openai_tool(model):
    response = openai_client.chat.completions.create(
        model=model,
        max_completion_tokens=192,
        messages=[{"role": "user", "content": "Use lookup_weather for Paris, then report the returned temperature."}],
        tools=[{"type": "function", "function": {"name": "lookup_weather", "description": "Look up current weather", "parameters": TOOL_SCHEMA}}],
        tool_choice={"type": "function", "function": {"name": "lookup_weather"}},
    )
    calls = response.choices[0].message.tool_calls or []
    if not calls:
        return {"ok": False, "phase": "tool_call", "output": response.choices[0].message.content}
    call = calls[0]
    arguments = json.loads(call.function.arguments)
    followup = openai_client.chat.completions.create(
        model=model,
        max_completion_tokens=128,
        messages=[
            {"role": "user", "content": "Use lookup_weather for Paris, then report the returned temperature."},
            response.choices[0].message.model_dump(exclude_none=True),
            {"role": "tool", "tool_call_id": call.id, "content": '{"city":"Paris","temperature_c":21,"rain":false}'},
        ],
        tools=[{"type": "function", "function": {"name": "lookup_weather", "description": "Look up current weather", "parameters": TOOL_SCHEMA}}],
    )
    text = (followup.choices[0].message.content or "").strip()
    return {"ok": call.function.name == "lookup_weather" and arguments.get("city", "").lower() == "paris" and "21" in text, "tool_name": call.function.name, "tool_input": arguments, "final_output": text[:240]}


def openai_vision(model):
    data_url = "data:image/jpeg;base64," + base64.b64encode(IMAGE_PATH.read_bytes()).decode()
    if model in CODEX_MODELS:
        response = openai_client.responses.create(
            model=model,
            max_output_tokens=512,
            input=[{"role": "user", "content": [
                {"type": "input_text", "text": "Describe the main vehicle and setting in this image. Do not guess from a filename."},
                {"type": "input_image", "image_url": data_url},
            ]}],
        )
        text = response.output_text.strip()
        lower = text.lower()
        return {"ok": bool(text) and ("bus" in lower or "公交" in text or "巴士" in text), "output": text[:500]}
    response = openai_client.chat.completions.create(
        model=model,
        max_completion_tokens=512,
        messages=[{"role": "user", "content": [
            {"type": "text", "text": "Describe the main vehicle and setting in this image. Do not guess from a filename."},
            {"type": "image_url", "image_url": {"url": data_url}},
        ]}],
    )
    text = (response.choices[0].message.content or "").strip()
    lower = text.lower()
    return {"ok": bool(text) and ("bus" in lower or "公交" in text or "巴士" in text), "output": text[:500]}


def openai_structured(model):
    if model in CODEX_MODELS:
        response = openai_client.responses.create(
            model=model,
            max_output_tokens=1024,
            input="Return Paris weather: 21 C and no rain.",
            text={"format": {"type": "json_schema", "name": "weather", "strict": True, "schema": SCHEMA}},
        )
        text = response.output_text.strip()
        try:
            parsed = json.loads(text)
        except json.JSONDecodeError as exc:
            return {"ok": False, "error_category": "response_validation", "validation_error": str(exc), "output": text[:500]}
        return {"ok": valid_weather(parsed), "output": parsed}
    response = openai_client.chat.completions.create(
        model=model,
        max_completion_tokens=192,
        messages=[{"role": "user", "content": "Return Paris weather: 21 C and no rain."}],
        response_format={"type": "json_schema", "json_schema": {"name": "weather", "strict": True, "schema": SCHEMA}},
    )
    text = (response.choices[0].message.content or "").strip()
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError as exc:
        return {"ok": False, "error_category": "response_validation", "validation_error": str(exc), "output": text[:500]}
    return {"ok": valid_weather(parsed), "output": parsed}


def openai_reasoning(model, effort):
    response = openai_client.responses.create(
        model=model,
        input="Find the smallest positive integer divisible by both 18 and 24 that leaves remainder 5 when divided by 7. Reply with only the integer.",
        max_output_tokens=512,
        reasoning={"effort": effort},
    )
    usage = summarize_usage(response.usage)
    text = response.output_text.strip()
    return {"ok": answer_is_432(text), "format_exact": text.replace(",", "") == "432", "effort": effort, "output": text[:240], "usage": usage}


def gemini_text(model):
    chat = gemini_client.chats.create(model=model, config=types.GenerateContentConfig(max_output_tokens=2048))
    chat.send_message("Remember the codeword ORBIT-73. Reply READY.")
    response = chat.send_message("What codeword did I ask you to remember? Reply with only it.")
    text = (response.text or "").strip()
    return {"ok": text.upper() == "ORBIT-73", "output": text[:240], "usage": summarize_usage(response.usage_metadata)}


def gemini_stream(model):
    text_parts = []
    events = 0
    finish = None
    for chunk in gemini_client.models.generate_content_stream(
        model=model,
        contents="Reply with exactly STREAM-OK.",
        config=types.GenerateContentConfig(max_output_tokens=96),
    ):
        events += 1
        if chunk.text:
            text_parts.append(chunk.text)
        if chunk.candidates and chunk.candidates[0].finish_reason:
            finish = str(chunk.candidates[0].finish_reason)
    text = "".join(text_parts).strip()
    return {"ok": bool(text) and finish is not None, "output": text[:240], "finish_reason": finish, "events": events}


def gemini_tool(model):
    declaration = types.FunctionDeclaration(name="lookup_weather", description="Look up current weather", parameters_json_schema=TOOL_SCHEMA)
    config = types.GenerateContentConfig(
        max_output_tokens=1024,
        tools=[types.Tool(function_declarations=[declaration])],
        tool_config=types.ToolConfig(function_calling_config=types.FunctionCallingConfig(mode="ANY", allowed_function_names=["lookup_weather"])),
        automatic_function_calling=types.AutomaticFunctionCallingConfig(disable=True),
    )
    response = gemini_client.models.generate_content(model=model, contents="Use lookup_weather for Paris.", config=config)
    calls = response.function_calls or []
    if not calls:
        return {"ok": False, "phase": "tool_call", "output": response.text}
    call = calls[0]
    arguments = dict(call.args or {})
    history = [
        types.Content(role="user", parts=[types.Part.from_text(text="Use lookup_weather for Paris, then report the returned temperature.")]),
        response.candidates[0].content,
        types.Content(role="user", parts=[types.Part.from_function_response(name=call.name, response={"city": "Paris", "temperature_c": 21, "rain": False})]),
    ]
    followup = gemini_client.models.generate_content(
        model=model,
        contents=history,
        config=types.GenerateContentConfig(max_output_tokens=1024, tools=[types.Tool(function_declarations=[declaration])]),
    )
    text = (followup.text or "").strip()
    return {"ok": call.name == "lookup_weather" and str(arguments.get("city", "")).lower() == "paris" and "21" in text, "tool_name": call.name, "tool_input": arguments, "final_output": text[:240]}


def gemini_vision(model):
    config = types.GenerateContentConfig(
        max_output_tokens=1536,
        response_modalities=["TEXT"],
        thinking_config=types.ThinkingConfig(thinking_level="LOW"),
    )
    response = gemini_client.models.generate_content(
        model=model,
        contents=["Describe the main vehicle and setting in this image. Do not guess from a filename.", types.Part.from_bytes(data=IMAGE_PATH.read_bytes(), mime_type="image/jpeg")],
        config=config,
    )
    text = (response.text or "").strip()
    lower = text.lower()
    return {"ok": bool(text) and ("bus" in lower or "公交" in text or "巴士" in text), "output": text[:500]}


def gemini_structured(model):
    response = gemini_client.models.generate_content(
        model=model,
        contents="Return Paris weather: 21 C and no rain.",
        config=types.GenerateContentConfig(response_mime_type="application/json", response_json_schema=GEMINI_SCHEMA, max_output_tokens=512, thinking_config=types.ThinkingConfig(thinking_level="LOW")),
    )
    text = response.text or ""
    try:
        parsed = response.parsed if isinstance(response.parsed, dict) else json.loads(text)
    except json.JSONDecodeError as exc:
        return {"ok": False, "error_category": "response_validation", "validation_error": str(exc), "output": text[:500]}
    return {"ok": valid_weather(parsed), "output": parsed}


def gemini_reasoning(model, effort):
    response = gemini_client.models.generate_content(
        model=model,
        contents="Find the smallest positive integer divisible by both 18 and 24 that leaves remainder 5 when divided by 7. Reply with only the integer.",
        config=types.GenerateContentConfig(
            max_output_tokens=4096,
            thinking_config=types.ThinkingConfig(include_thoughts=True, thinking_level=effort.upper()),
        ),
    )
    thought_parts = []
    if response.candidates:
        for part in response.candidates[0].content.parts or []:
            if part.thought:
                thought_parts.append(part.text or "")
    text = (response.text or "").strip()
    return {"ok": answer_is_432(text), "format_exact": text.replace(",", "") == "432", "effort": effort, "output": text[:240], "thinking_parts": len(thought_parts), "thinking_chars": sum(map(len, thought_parts)), "usage": summarize_usage(response.usage_metadata)}


def image_dimensions(data):
    if data.startswith(b"\x89PNG\r\n\x1a\n") and len(data) >= 24:
        return "png", struct.unpack(">II", data[16:24])
    if data.startswith(b"\xff\xd8"):
        index = 2
        while index + 9 < len(data):
            if data[index] != 0xFF:
                index += 1
                continue
            marker = data[index + 1]
            length = int.from_bytes(data[index + 2:index + 4], "big")
            if marker in range(0xC0, 0xC4):
                return "jpeg", (int.from_bytes(data[index + 7:index + 9], "big"), int.from_bytes(data[index + 5:index + 7], "big"))
            index += 2 + length
    return None, None


def gemini_image_generation(model):
    response = gemini_client.models.generate_content(
        model=model,
        contents="Create one clean square editorial illustration of a red city bus driving past a green park under a blue sky. No text.",
        config=types.GenerateContentConfig(
            response_modalities=["IMAGE"],
            image_config=types.ImageConfig(aspect_ratio="1:1"),
        ),
    )
    GENERATED_DIR.mkdir(exist_ok=True)
    images = []
    for candidate in response.candidates or []:
        for part in candidate.content.parts or []:
            inline = part.inline_data
            if inline and inline.data:
                data = inline.data if isinstance(inline.data, bytes) else base64.b64decode(inline.data)
                fmt, dimensions = image_dimensions(data)
                path = GENERATED_DIR / f"{model}.{fmt or 'bin'}"
                path.write_bytes(data)
                images.append({"path": str(path), "mime_type": inline.mime_type, "bytes": len(data), "format": fmt, "dimensions": dimensions})
    return {"ok": bool(images) and all(item["dimensions"] for item in images), "images": images}


def gemini_image_edit(model):
    response = gemini_client.models.generate_content(
        model=model,
        contents=[
            "Edit this photo into a clean watercolor illustration while preserving the blue bus and city street. Return one image.",
            types.Part.from_bytes(data=IMAGE_PATH.read_bytes(), mime_type="image/jpeg"),
        ],
        config=types.GenerateContentConfig(
            response_modalities=["IMAGE"],
            image_config=types.ImageConfig(aspect_ratio="4:3"),
        ),
    )
    GENERATED_DIR.mkdir(exist_ok=True)
    images = []
    for candidate in response.candidates or []:
        for part in candidate.content.parts or []:
            inline = part.inline_data
            if inline and inline.data:
                data = inline.data if isinstance(inline.data, bytes) else base64.b64decode(inline.data)
                fmt, dimensions = image_dimensions(data)
                path = GENERATED_DIR / f"{model}-edited.{fmt or 'bin'}"
                path.write_bytes(data)
                images.append({"path": str(path), "mime_type": inline.mime_type, "bytes": len(data), "format": fmt, "dimensions": dimensions})
    return {"ok": bool(images) and all(item["dimensions"] for item in images), "images": images}


def capabilities_for(model):
    if model in ANTHROPIC_MODELS:
        return [
            ("chat", lambda: anthropic_text(model)),
            ("stream", lambda: anthropic_stream(model)),
            ("tool", lambda: anthropic_tool(model)),
            ("vision", lambda: anthropic_vision(model)),
            ("structured", lambda: anthropic_structured(model)),
            ("reasoning_low", lambda: anthropic_reasoning(model, "low")),
            ("reasoning_high", lambda: anthropic_reasoning(model, "high")),
        ]
    if model in GEMINI_MODELS:
        cases = [
            ("chat", lambda: gemini_text(model)),
            ("stream", lambda: gemini_stream(model)),
            ("tool", lambda: gemini_tool(model)),
            ("vision", lambda: gemini_vision(model)),
            ("structured", lambda: gemini_structured(model)),
            ("reasoning_low", lambda: gemini_reasoning(model, "low")),
            ("reasoning_high", lambda: gemini_reasoning(model, "high")),
        ]
        if model == "gemini-3.1-flash-image":
            cases.append(("image_generation", lambda: gemini_image_generation(model)))
            cases.append(("image_edit", lambda: gemini_image_edit(model)))
        return cases
    return [
        ("chat", lambda: openai_text(model)),
        ("stream", lambda: openai_stream(model)),
        ("tool", lambda: openai_tool(model)),
        ("vision", lambda: openai_vision(model)),
        ("structured", lambda: openai_structured(model)),
        ("reasoning_low", lambda: openai_reasoning(model, "low")),
        ("reasoning_high", lambda: openai_reasoning(model, "high")),
    ]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model")
    parser.add_argument("--provider", choices=["anthropic", "gemini", "openai"])
    parser.add_argument("--capability")
    args = parser.parse_args()
    models = ANTHROPIC_MODELS + GEMINI_MODELS + OPENAI_MODELS
    if args.provider == "anthropic":
        models = ANTHROPIC_MODELS
    elif args.provider == "gemini":
        models = GEMINI_MODELS
    elif args.provider == "openai":
        models = OPENAI_MODELS
    if args.model:
        models = [args.model]
    for model in models:
        sdk = "anthropic" if model in ANTHROPIC_MODELS else "google-genai" if model in GEMINI_MODELS else "openai"
        for capability, call in capabilities_for(model):
            if args.capability and capability != args.capability:
                continue
            run_case(model, sdk, capability, call)


if __name__ == "__main__":
    main()
