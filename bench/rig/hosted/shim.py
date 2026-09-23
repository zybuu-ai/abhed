"""LiteLLM proxy hook for a hosted benchmark run.

Holds every request to the rig's per-turn output limit, whatever the harness
asked for, and turns empty or null message content into an empty string,
which some hosted APIs, watsonx among them, reject.
"""
import os

try:
    from litellm.integrations.custom_logger import CustomLogger
except ImportError:  # the pure function below is tested without litellm
    CustomLogger = object

# The same variable and default as MAX_OUTPUT in rig.py.
MAX_OUTPUT = int(os.environ.get("ABHED_BENCH_MAX_OUTPUT", "8192"))


def normalise(data, cap=MAX_OUTPUT):
    for m in data.get("messages") or []:
        c = m.get("content")
        if c is None or c == []:
            m["content"] = ""
        elif isinstance(c, list):
            parts = [p.get("text", "") for p in c if isinstance(p, dict) and p.get("type") == "text"]
            if len(parts) == len(c):
                m["content"] = "\n".join(parts)
    for key in ("max_tokens", "max_completion_tokens"):
        if data.get(key):
            data[key] = min(int(data[key]), cap)
    if not data.get("max_tokens") and not data.get("max_completion_tokens"):
        data["max_tokens"] = cap
    return data


class Shim(CustomLogger):
    async def async_pre_call_hook(self, user_api_key_dict, cache, data, call_type):
        return normalise(data)


shim = Shim()
