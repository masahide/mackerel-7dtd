"""Small generated Docker/OCI artifacts; no Docker daemon or live paths."""
import gzip
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from suzume_update import ArchiveStore, Blocked, OCI_CONFIG, OCI_GZIP, OCI_INDEX, OCI_MANIFEST, OCI_TAR
from test_suzume_update import make_image


def sha(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def blob_name(digest):
    return "blobs/sha256/" + digest.split(":", 1)[1]


def layer_bytes(contents=b"fixture"):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w") as tar:
        item = tarfile.TarInfo("fixture.txt")
        item.size = len(contents)
        tar.addfile(item, io.BytesIO(contents))
    return stream.getvalue()


def oci_entries(codec="gzip", change_config=lambda _: None, change_manifest=lambda _: None,
                change_index=lambda _: None, change_docker=lambda _: None, payload=None,
                payloads=None, corrupt_gzip=False):
    unpacked = payloads if payloads is not None else [layer_bytes() if payload is None else payload]
    stored = [gzip.compress(p, mtime=0) if codec == "gzip" else p for p in unpacked]
    if corrupt_gzip:
        stored[0] = stored[0][:-8] + bytes([stored[0][-8] ^ 1]) + stored[0][-7:]
    layer_ids = [sha(p) for p in stored]
    config = {"architecture": "amd64", "os": "linux", "rootfs": {"type": "layers", "diff_ids": [sha(p) for p in unpacked]}}
    change_config(config)
    config_raw = json.dumps(config).encode()
    config_id = sha(config_raw)
    manifest = {"schemaVersion": 2, "mediaType": OCI_MANIFEST,
                "config": {"mediaType": OCI_CONFIG, "digest": config_id, "size": len(config_raw)},
                "layers": [{"mediaType": OCI_GZIP if codec == "gzip" else OCI_TAR,
                            "digest": layer_id, "size": len(data)} for layer_id, data in zip(layer_ids, stored)]}
    change_manifest(manifest)
    manifest_raw = json.dumps(manifest).encode()
    image_id = sha(manifest_raw)
    index = {"schemaVersion": 2, "mediaType": OCI_INDEX,
             "manifests": [{"mediaType": OCI_MANIFEST, "digest": image_id, "size": len(manifest_raw)}]}
    change_index(index)
    docker = [{"Config": blob_name(config_id), "Layers": [blob_name(i) for i in layer_ids], "RepoTags": None}]
    change_docker(docker)
    entries = {"oci-layout": b'{"imageLayoutVersion":"1.0.0"}', "index.json": json.dumps(index).encode(),
               "manifest.json": json.dumps(docker).encode(), blob_name(image_id): manifest_raw,
               blob_name(config_id): config_raw}
    entries.update({blob_name(i): data for i, data in zip(layer_ids, stored)})
    return entries, image_id, config_id, layer_ids[0]


def write_archive(path, entries, extra=None):
    with tarfile.open(path, "w") as tar:
        for name, data in entries.items():
            item = tarfile.TarInfo(name)
            item.size = len(data)
            tar.addfile(item, io.BytesIO(data))
        if extra:
            tar.addfile(extra)


class ImageArchiveTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "image.tar"

    def check_fixture(self, **kwargs):
        entries, image_id, *_ = oci_entries(**kwargs)
        write_archive(self.path, entries)
        ArchiveStore.validate_image(self.path, image_id)

    def test_oci_plain_gzip_and_pure_layout_accept_manifest_identity(self):
        for codec in ["plain", "gzip"]:
            with self.subTest(codec=codec):
                entries, image_id, config_id, _ = oci_entries(codec=codec)
                self.assertNotEqual(image_id, config_id)
                write_archive(self.path, entries)
                ArchiveStore.validate_image(self.path, image_id)
                with self.assertRaisesRegex(Blocked, "IMAGE_ID_MISMATCH"):
                    ArchiveStore.validate_image(self.path, config_id)
                del entries["manifest.json"]
                write_archive(self.path, entries)
                ArchiveStore.validate_image(self.path, image_id)

    def test_manifest_config_and_layer_tampering_fail_closed(self):
        for target in ["manifest", "config", "layer"]:
            with self.subTest(target=target):
                entries, image_id, config_id, layer_id = oci_entries()
                key = blob_name({"manifest": image_id, "config": config_id, "layer": layer_id}[target])
                entries[key] = entries[key][:-1] + bytes([entries[key][-1] ^ 1])
                write_archive(self.path, entries)
                with self.assertRaisesRegex(Blocked, "IMAGE_BLOB_DIGEST_MISMATCH"):
                    ArchiveStore.validate_image(self.path, image_id)

    def test_descriptor_size_digest_type_and_external_data_rejected(self):
        cases = [lambda m: m["config"].update(size=True), lambda m: m["config"].update(size=-1),
                 lambda m: m["config"].update(size=m["config"]["size"] + 1),
                 lambda m: m["config"].update(digest="sha512:" + "0" * 128),
                 lambda m: m["layers"][0].update(mediaType=OCI_TAR + "+zstd"),
                 lambda m: m["layers"][0].update(urls=["https://example.invalid/layer"]),
                 lambda m: m["config"].update(data="e30=")]
        for change in cases:
            with self.subTest(change=change), self.assertRaises(Blocked):
                self.check_fixture(change_manifest=change)

    def test_diff_id_order_and_count_are_independent_of_compressed_digest(self):
        cases = [lambda c: c["rootfs"].update(diff_ids=["sha256:" + "0" * 64]),
                 lambda c: c["rootfs"].update(diff_ids=[]),
                 lambda c: c["rootfs"].update(diff_ids="invalid"),
                 lambda c: c["rootfs"].update(type="unknown")]
        for change in cases:
            with self.subTest(change=change), self.assertRaises(Blocked):
                self.check_fixture(change_config=change)
        payloads = [layer_bytes(), layer_bytes(b"changed")]
        self.check_fixture(payloads=payloads)
        with self.assertRaisesRegex(Blocked, "IMAGE_LAYER_MISMATCH"):
            self.check_fixture(payloads=payloads, change_config=lambda c: c["rootfs"]["diff_ids"].reverse())
        # Reused empty/identical layers occur in real Docker-save artifacts.
        self.check_fixture(payloads=[layer_bytes(), layer_bytes()])

    def test_gzip_crc_truncation_and_expansion_limit_fail_closed(self):
        with self.assertRaises(Blocked): self.check_fixture(corrupt_gzip=True)
        with patch("suzume_update.MAX_LAYER_EXPANDED", 1024), self.assertRaisesRegex(Blocked, "IMAGE_LAYER_TOO_LARGE"):
            self.check_fixture()
        header = tarfile.TarInfo("truncated")
        header.size = 10000
        with self.assertRaises(Blocked): self.check_fixture(payload=header.tobuf() + b"short")

    def test_legacy_and_oci_entrypoints_must_describe_same_image(self):
        for change in [lambda d: d[0].update(Config="unrelated.json"),
                       lambda d: d[0].update(Layers=[]), lambda d: d.append(dict(d[0]))]:
            with self.subTest(change=change), self.assertRaises(Blocked):
                self.check_fixture(change_docker=change)

    def test_multiple_nested_unknown_and_mismatched_index_rejected(self):
        changes = [lambda i: i["manifests"].append(dict(i["manifests"][0])),
                   lambda i: i["manifests"][0].update(mediaType=OCI_INDEX),
                   lambda i: i.update(schemaVersion=True),
                   lambda i: i.update(mediaType="application/unknown")]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(Blocked):
                self.check_fixture(change_index=change)

    def test_matching_hashes_do_not_make_unreadable_tar_valid(self):
        for codec in ["plain", "gzip"]:
            with self.subTest(codec=codec), self.assertRaises(Blocked):
                self.check_fixture(codec=codec, payload=b"not a tar despite matching hashes")

    def test_duplicate_json_keys_missing_blob_and_unsafe_outer_members_rejected(self):
        for kind in ["json", "missing", "duplicate", "escape", "symlink"]:
            with self.subTest(kind=kind):
                entries, image_id, _, layer_id = oci_entries()
                extra = None
                if kind == "json": entries["oci-layout"] = b'{"imageLayoutVersion":"1.0.0","imageLayoutVersion":"1.0.0"}'
                if kind == "missing": del entries[blob_name(layer_id)]
                if kind in ["duplicate", "escape", "symlink"]:
                    extra = tarfile.TarInfo("index.json" if kind == "duplicate" else "../escape" if kind == "escape" else "link")
                    if kind == "symlink": extra.type, extra.linkname = tarfile.SYMTYPE, "index.json"
                write_archive(self.path, entries, extra)
                with self.assertRaises(Blocked): ArchiveStore.validate_image(self.path, image_id)

    def test_legacy_docker_save_still_accepts_and_rejects_wrong_identity_or_layer(self):
        image_id = make_image(self.path)
        ArchiveStore.validate_image(self.path, image_id)
        with self.assertRaisesRegex(Blocked, "IMAGE_ID_MISMATCH"):
            ArchiveStore.validate_image(self.path, "sha256:" + "0" * 64)
        make_image(self.path, wrong_layer=True)
        with self.assertRaisesRegex(Blocked, "IMAGE_LAYER_MISMATCH"):
            ArchiveStore.validate_image(self.path, image_id)


if __name__ == "__main__":
    unittest.main()
