import os
import sys
import tempfile
from pathlib import Path

os.environ["DATA_DIR"] = tempfile.mkdtemp(prefix="tpcbcr-test-")
os.environ["AUTH_MODE"] = "dev"

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
