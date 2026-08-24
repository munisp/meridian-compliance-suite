import os
import sys
from pathlib import Path

os.environ.setdefault("AUTH_MODE", "dev")
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "packages" / "py"))
