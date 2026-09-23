"""LiteLLM proxy hook for a hosted benchmark run.

Gives every request the same output limit when the harness names none (the
rig's MAX_OUTPUT), and turns empty or null message content into an empty
string, which some hosted APIs, watsonx among them, reject.
"""
import os

from litellm.integrations.custom_logger import CustomLogger

MAX_OUTPUT = int(os.environ.get("ABHED_BENCH_MAX_OUTPUT", "8192"))


class Shim(CustomLogger):
    async def async_pre_call_hook(self, user_api_key_dict, cache, data, call_type):
        for m in data.get("messages") or []:
            c = m.get("content")
            if c is None or c == []:
                m["content"] = ""
            elif isinstance(c, list):
                parts = [p.get("text", "") for p in c if isinstance(p, dict) and p.get("type") == "text"]
                if len(parts) == len(c):
                    m["content"] = "\n".join(parts)
        if not data.get("max_tokens"):
            data["max_tokens"] = MAX_OUTPUT
        return data


shim = Shim()
