"""
IndexTTS HTTP server — kokoro-compatible TTS endpoint for the tachi device channel.

API (compatible with channel/device/device.go's Kokoro wrapper):

    POST /tts
        Body: {"text": "...", "voice": "...", "speed": 1.05}
        Resp: audio/wav bytes (raw PCM wav, base64-encoded by the caller)
    GET  /health -> {"status": "ok", "model_loaded": true}

Environment variables:

    INDEX_TTS_DIR         IndexTTS project root (default: ~/repos/tts/index-tts)
    INDEX_TTS_MODEL_DIR   checkpoint dir with config.yaml/gpt.pth (default: $INDEX_TTS_DIR/checkpoints)
    TTS_PROMPTS_DIR       reference voice dir (default: <this file dir>/prompts)
    TTS_HOST              bind host (default: 0.0.0.0)
    TTS_PORT              bind port (default: 8888)
    TTS_MAX_TEXT          max text chars (default: 500, mirrors device.go)
    TTS_PRELOAD           "1" preload model at startup (default: 1)

Voice:

    固定使用塔奇克马音色（prompts/tachikoma/ref_tachikoma_12s.wav），
    请求中的 voice 字段被忽略。
    语速固定为放慢 15%（duration_factor = 1.15），请求中的 speed 字段被忽略。
"""

from __future__ import annotations

import os
import re
import sys
import tempfile
import threading
from contextlib import asynccontextmanager
from pathlib import Path

# ---------------------------------------------------------------------------
# configuration
# ---------------------------------------------------------------------------

INDEX_TTS_DIR = Path(os.environ.get("INDEX_TTS_DIR", str(Path.home() / "repos/tts/index-tts")))
MODEL_DIR = Path(os.environ.get("INDEX_TTS_MODEL_DIR", str(INDEX_TTS_DIR / "checkpoints")))
PROMPTS_DIR = Path(os.environ.get("TTS_PROMPTS_DIR", str(Path(__file__).resolve().parent / "prompts")))
# 固定使用塔奇克马音色（忽略请求中的 voice 字段）
TACHIKOMA_PROMPT = PROMPTS_DIR / "tachikoma" / "ref_tachikoma_12s.wav"
HOST = os.environ.get("TTS_HOST", "0.0.0.0")
PORT = int(os.environ.get("TTS_PORT", "8888"))
MAX_TEXT = int(os.environ.get("TTS_MAX_TEXT", "500"))
PRELOAD = os.environ.get("TTS_PRELOAD", "1") == "1"

# make indextts importable even when it is not installed into this venv
sys.path.insert(0, str(INDEX_TTS_DIR))
_sp = INDEX_TTS_DIR / ".venv" / "lib" / f"python{sys.version_info.major}.{sys.version_info.minor}" / "site-packages"
if _sp.is_dir():
    sys.path.insert(0, str(_sp))

from fastapi import FastAPI
from fastapi.responses import JSONResponse, Response  # noqa: E402
from indextts.infer_v2_5 import IndexTTS2  # noqa: E402

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def resolve_prompt(_voice: str | None) -> str:
    """固定使用塔奇克马参考音频（忽略传入的音色名）。"""
    if not TACHIKOMA_PROMPT.is_file():
        raise FileNotFoundError(f"tachikoma reference audio not found: {TACHIKOMA_PROMPT}")
    return str(TACHIKOMA_PROMPT)


def detect_lang(text: str) -> str:
    """Heuristic: kana -> JA, CJK with latin words -> zh/en (mixed), CJK -> ZH, else EN."""
    if re.search(r"[\u3040-\u30ff]", text):
        return "JA"
    if re.search(r"[\u4e00-\u9fff]", text):
        # 中英混合（中文为主）→ IndexTTS 的 zh/en 混合模式
        if re.search(r"[a-zA-Z]", text):
            return "zh/en"
        return "ZH"
    return "EN"


def clean_text(text: str) -> str:
    # mirror device.go: collapse newlines/tabs, cap length
    text = re.sub(r"[\n\r\t]", " ", text)
    return text[:MAX_TEXT]


# ---------------------------------------------------------------------------
# model holder (singleton, lazy-load with lock)
# ---------------------------------------------------------------------------


class ModelHolder:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.tts: IndexTTS2 | None = None

    def ensure(self) -> IndexTTS2:
        if self.tts is not None:
            return self.tts
        with self._lock:
            if self.tts is None:
                print(f">> loading IndexTTS2 from {MODEL_DIR} ...", flush=True)
                self.tts = IndexTTS2(
                    cfg_path=str(MODEL_DIR / "config.yaml"),
                    model_dir=str(MODEL_DIR),
                    use_bf16=True,
                    use_cuda_kernel=False,
                    use_torch_compile=False,
                    use_qwen_emo=True,
                )
                print(">> model ready", flush=True)
            return self.tts

    def synth(self, text: str, lang: str | None) -> bytes:
        tts = self.ensure()
        prompt = resolve_prompt(None)  # 固定塔奇克马音色
        lang = lang or detect_lang(text)
        if lang.lower() in ("zh/en", "en/zh"):
            lang = lang.lower()  # 混合语言标记保持小写（tokenizer 注册的就是小写）
        else:
            lang = lang.upper()
        # 固定语速：放慢 15%（用户偏好；忽略请求中的 speed）
        df = 1.15
        with self._lock:  # single-flight: model state is not concurrency-safe
            fd, out = tempfile.mkstemp(suffix=".wav", dir=PROMPTS_DIR)
            os.close(fd)
            try:
                tts.infer(
                    spk_audio_prompt=prompt,
                    text=text,
                    lang=lang,
                    output_path=out,
                    duration_factor=df,
                    verbose=False,
                    text_normalization=True,
                )
                return Path(out).read_bytes()
            finally:
                Path(out).unlink(missing_ok=True)


holder = ModelHolder()

# ---------------------------------------------------------------------------
# app
# ---------------------------------------------------------------------------


@asynccontextmanager
async def lifespan(_app: FastAPI):
    if PRELOAD:
        try:
            holder.ensure()
        except Exception as e:  # keep server alive; first request will retry
            print(f">> model preload failed: {e}", flush=True)
    yield


app = FastAPI(title="tts-server", version="0.1.0", lifespan=lifespan)


@app.get("/health")
def health():
    return {"status": "ok", "model_loaded": holder.tts is not None, "voice": "tachikoma"}


@app.post("/tts")
def tts(req: dict | None = None):
    """Kokoro-compatible endpoint: {"text","voice","speed"} -> audio/wav bytes.

    voice 与 speed 字段均被忽略：固定塔奇克马音色，语速固定放慢 15%。
    """
    req = req or {}
    text = clean_text(str(req.get("text", "")).strip())
    if not text:
        return JSONResponse({"error": "empty text"}, status_code=400)
    lang = req.get("lang")  # optional override
    try:
        wav = holder.synth(text, lang)
    except FileNotFoundError as e:
        return JSONResponse({"error": str(e)}, status_code=400)
    except Exception as e:
        return JSONResponse({"error": f"tts failed: {e}"}, status_code=500)
    return Response(content=wav, media_type="audio/wav")


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host=HOST, port=PORT)
