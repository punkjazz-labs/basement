import importlib.util
import json
import pathlib
import subprocess
import sys
import tempfile
import types
import unittest


MODULE = pathlib.Path(__file__).parents[1] / "basement_ablit.py"
SPEC = importlib.util.spec_from_file_location("basement_ablit_test", MODULE)
ablit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ablit)


class ManifestTest(unittest.TestCase):
    def setUp(self):
        self.previous = ablit.MANIFEST_PATH
        self.directory = tempfile.TemporaryDirectory()
        ablit.MANIFEST_PATH = pathlib.Path(self.directory.name) / "manifest.json"

    def tearDown(self):
        ablit.MANIFEST_PATH = self.previous
        self.directory.cleanup()

    def test_manifest_requires_every_layer(self):
        ablit.MANIFEST_PATH.write_text(json.dumps({"version": 1, "layers": {"15": {}}}))
        with self.assertRaises(ablit.AbliterationError):
            ablit._manifest()

    def test_runtime_targets_the_mtp_block_and_has_no_projection_fallback(self):
        source = MODULE.read_text()
        self.assertIn('layers["45"].mtp_block', source)
        self.assertNotIn("ABLIT_METHOD", source)
        self.assertNotIn("apply_to_o_proj", source)

    def test_base_and_mtp_copy_every_expected_target(self):
        class NoGrad:
            def __enter__(self):
                return self
            def __exit__(self, *unused):
                return False

        class Donor:
            shape = (2, 4)
            def __getitem__(self, unused):
                return self
            def to(self, **unused):
                return self

        class Weight:
            shape = (2, 2)
            device = "fake"
            dtype = "fake"
            def __init__(self):
                self.data = self
                self.copies = 0
            def copy_(self, unused):
                self.copies += 1

        class Block:
            def __init__(self):
                self.self_attn = types.SimpleNamespace(o_proj=types.SimpleNamespace(weight=Weight()))

        original = (ablit._manifest, ablit._tensor_parallel, ablit._donor, sys.modules.get("torch"))
        entries = {str(layer): {"sha256": "0" * 64} for layer in ablit.LAYERS}
        ablit._manifest = lambda: entries
        ablit._tensor_parallel = lambda: (1, 2)
        ablit._donor = lambda layer, entry: Donor()
        sys.modules["torch"] = types.SimpleNamespace(no_grad=NoGrad)
        try:
            base = types.SimpleNamespace(layers={layer: Block() for layer in range(15, 45)})
            ablit._apply(base, False)
            self.assertTrue(all(base.layers[layer].self_attn.o_proj.weight.copies == 1 for layer in range(15, 45)))
            mtp_block = Block()
            mtp = types.SimpleNamespace(model=types.SimpleNamespace(layers={"45": types.SimpleNamespace(mtp_block=mtp_block)}))
            ablit._apply(mtp, True)
            self.assertEqual(mtp_block.self_attn.o_proj.weight.copies, 1)
        finally:
            ablit._manifest, ablit._tensor_parallel, ablit._donor = original[:3]
            if original[3] is None:
                del sys.modules["torch"]
            else:
                sys.modules["torch"] = original[3]

    def test_missing_mtp_predictor_fails_without_touching_base(self):
        original = (ablit._manifest, ablit._tensor_parallel)
        ablit._manifest = lambda: {str(layer): {} for layer in ablit.LAYERS}
        ablit._tensor_parallel = lambda: (0, 2)
        try:
            with self.assertRaises(ablit.AbliterationError):
                ablit._apply(types.SimpleNamespace(layers={}), True)
        finally:
            ablit._manifest, ablit._tensor_parallel = original

    def test_loaders_are_wrapped_only_after_the_original_returns(self):
        events = []

        class Base:
            def load_weights(self, weights):
                events.append(("base", weights))
                return {"base"}

        class MTP:
            def load_weights(self, weights):
                events.append(("mtp", weights))
                return {"mtp"}

        original_apply = ablit._apply
        ablit._apply = lambda model, is_mtp: events.append(("apply", type(model).__name__, is_mtp))
        try:
            ablit._wrap(Base, False)
            ablit._wrap(MTP, True)
            self.assertEqual(Base().load_weights("a"), {"base"})
            self.assertEqual(MTP().load_weights("b"), {"mtp"})
        finally:
            ablit._apply = original_apply
        self.assertEqual(events, [("base", "a"), ("apply", "Base", False), ("mtp", "b"), ("apply", "MTP", True)])

    def test_pth_bootstrap_installs_lazily_and_exits_on_failure(self):
        template = (MODULE.parent / "basement_ablit.pth").read_text()
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            hook = root / "hook"
            hook.mkdir()
            pth = root / "test.pth"
            pth.write_text(template.replace("/opt/basement/ablit", str(hook)))
            command = [sys.executable, "-c", "import site; site.addpackage(%r, 'test.pth', set()); print('continued')" % str(root)]
            (hook / "basement_ablit.py").write_text("def install(): pass\n")
            ok = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(ok.returncode, 0, ok.stderr)
            self.assertIn("continued", ok.stdout)
            (hook / "basement_ablit.py").write_text("raise RuntimeError('broken')\n")
            failed = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(failed.returncode, 72, (failed.stdout, failed.stderr))


if __name__ == "__main__":
    unittest.main()
