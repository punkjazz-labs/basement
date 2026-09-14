"""Pinned GLM-5.3 Flash donor transplant loaded from basement_ablit.pth."""

import hashlib
import importlib.abc
import importlib.machinery
import json
import logging
from pathlib import Path

ABLIT_DIR = Path("/abliteration")
MANIFEST_PATH = Path(__file__).with_name("manifest.json")
LAYERS = tuple(range(15, 46))
LOGGER = logging.getLogger("vllm.basement.abliteration")


class AbliterationError(RuntimeError):
    pass


def _tensor_parallel():
    from vllm.distributed import (get_tensor_model_parallel_rank,
                                  get_tensor_model_parallel_world_size)
    return get_tensor_model_parallel_rank(), get_tensor_model_parallel_world_size()


def _manifest():
    try:
        data = json.loads(MANIFEST_PATH.read_text())
    except Exception as exc:
        raise AbliterationError("abliteration manifest is missing or invalid") from exc
    if data.get("version") != 1 or data.get("layers") is None:
        raise AbliterationError("abliteration manifest has an unsupported shape")
    entries = data["layers"]
    required = {str(layer) for layer in LAYERS}
    if set(entries) != required:
        raise AbliterationError("abliteration manifest must cover exactly layers 15 through 45")
    return entries


def _donor(layer, entry):
    import torch

    if entry.get("dtype") != "BF16":
        raise AbliterationError("abliteration donor L%d is not BF16" % layer)
    shape = entry.get("shape")
    if not (isinstance(shape, list) and len(shape) == 2 and all(isinstance(v, int) and v > 0 for v in shape)):
        raise AbliterationError("abliteration donor L%d has invalid shape" % layer)
    path = ABLIT_DIR / ("L%d.bin" % layer)
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise AbliterationError("abliteration donor L%d is missing" % layer) from exc
    if len(raw) != shape[0] * shape[1] * 2:
        raise AbliterationError("abliteration donor L%d has an invalid byte count" % layer)
    if hashlib.sha256(raw).hexdigest() != entry.get("sha256"):
        raise AbliterationError("abliteration donor L%d digest does not match the manifest" % layer)
    return torch.frombuffer(bytearray(raw), dtype=torch.bfloat16).reshape(shape)


def _apply(model, is_mtp):
    entries = _manifest()
    rank, world = _tensor_parallel()
    if world != 2 or rank not in (0, 1):
        raise AbliterationError("abliteration requires exactly two tensor-parallel ranks")
    if is_mtp:
        predictor = getattr(model, "model", None)
        layers = getattr(predictor, "layers", None)
        if layers is None:
            raise AbliterationError("GLM MTP model does not expose its predictor layers")
        try:
            targets = ((45, layers["45"].mtp_block),)
        except (KeyError, AttributeError) as exc:
            raise AbliterationError("GLM MTP model does not expose layer 45") from exc
    else:
        layers = getattr(model, "layers", None)
        if layers is None:
            raise AbliterationError("GLM model does not expose its layers")
        targets = tuple((layer, layers[layer]) for layer in range(15, 45))
    for layer, block in targets:
        projection = getattr(getattr(block, "self_attn", None), "o_proj", None)
        weight = getattr(projection, "weight", None)
        if weight is None or len(weight.shape) != 2:
            raise AbliterationError("GLM layer %d does not expose a 2-D o_proj weight" % layer)
        donor = _donor(layer, entries[str(layer)])
        rows, full_columns = donor.shape
        local_columns = weight.shape[1]
        if rows != weight.shape[0] or full_columns != local_columns * world:
            raise AbliterationError("GLM layer %d donor shape does not match the TP-local o_proj" % layer)
        start = rank * local_columns
        shard = donor[:, start:start + local_columns].to(device=weight.device, dtype=weight.dtype)
        with __import__("torch").no_grad():
            weight.data.copy_(shard)
        LOGGER.info("abliteration transplanted layer=%d tp_rank=%d donor_sha256=%s", layer, rank, entries[str(layer)]["sha256"])


def _wrap(cls, is_mtp):
    original = cls.load_weights
    if getattr(original, "_basement_abliteration", False):
        return

    def load_weights(self, *args, **kwargs):
        result = original(self, *args, **kwargs)
        _apply(self, is_mtp)
        return result

    load_weights._basement_abliteration = True
    cls.load_weights = load_weights


class _Loader:
    def __init__(self, loader, class_name, is_mtp):
        self.loader = loader
        self.class_name = class_name
        self.is_mtp = is_mtp

    def create_module(self, spec):
        create = getattr(self.loader, "create_module", None)
        return create(spec) if create is not None else None

    def exec_module(self, module):
        self.loader.exec_module(module)
        target = getattr(module, self.class_name, None)
        if target is None:
            raise RuntimeError("the pinned image does not expose " + self.class_name)
        _wrap(target, self.is_mtp)


class _Finder(importlib.abc.MetaPathFinder):
    TARGETS = {
        "vllm.models.glm5next.nvidia.model": ("Glm5NextModel", False),
        "vllm.models.glm5next.nvidia.mtp": ("Glm5NextMTP", True),
    }

    def find_spec(self, fullname, path=None, target=None):
        match = self.TARGETS.get(fullname)
        if match is None:
            return None
        spec = importlib.machinery.PathFinder.find_spec(fullname, path)
        if spec is None or spec.loader is None:
            raise RuntimeError("the pinned image omits " + fullname)
        spec.loader = _Loader(spec.loader, *match)
        return spec


def install():
    if not any(isinstance(finder, _Finder) for finder in __import__("sys").meta_path):
        __import__("sys").meta_path.insert(0, _Finder())
