"""Run Jarvis's CallGraphGenerator on one fixture and report which functions it entered.

usage: python -I jarvis_probe.py FIXTURE.py PACKAGE_DIR [--precision]
The Jarvis tool directory must already be on sys.path (the venv's .pth file does that).
"""
import json
import sys

from jarvis import CallGraphGenerator  # from the Jarvis tool directory via .pth
from machinery import gol

fixture, package = sys.argv[1], sys.argv[2]
precision = "--precision" in sys.argv[3:]
cg = CallGraphGenerator([fixture], package, decy=False, precision=precision, moduleEntry=None)
cg.analyze()
entered = sorted(k for k in gol._global_dict if isinstance(k, str) and k.startswith("fixture"))
print(json.dumps({
    "moduleEntry": cg.moduleEntry,
    "entered_counts": {k: gol.get_value(k) for k in entered},
    "cg": {k: sorted(v) for k, v in cg.output().items()},
}, indent=1))
