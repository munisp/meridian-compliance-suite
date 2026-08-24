import os
import sys
import tempfile
from pathlib import Path

_TMP = tempfile.mkdtemp(prefix="str-filing-test-")

os.environ.setdefault("AUTH_MODE", "dev")
os.environ.setdefault("PROFILE", "dev")  # B2 #17: dev fallbacks are dev-profile only
os.environ["DATA_DIR"] = _TMP
os.environ.setdefault("STR_DATABASE_URL", f"sqlite:///{_TMP}/test.db")  # env override allows real-PG runs
os.environ["STR_NFIU_ADAPTER"] = "sim"          # SIM transport, tagged SIM
os.environ["STR_WORKER_ENABLED"] = "false"      # tests drive the worker
os.environ["STR_MAX_ATTEMPTS"] = "3"
os.environ["STR_RETRY_BASE_SECONDS"] = "0"      # no real sleeping in tests

svc = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(svc))
sys.path.insert(0, str(svc.parents[1] / "packages" / "py"))
