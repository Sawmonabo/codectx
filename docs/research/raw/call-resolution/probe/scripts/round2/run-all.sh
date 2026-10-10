#!/usr/bin/env bash
# Reproduces every r2 tool run. Each run waits for the shared heavy-workload lock.
# Prerequisites (already done once): PyCG and Jarvis venvs (Python 3.10, setuptools<81),
# a .pth in the Jarvis venv pointing at third_party/jarvis/src/Jarvis/tool/Jarvis,
# Soot 4.7.1 jar, Maven classpaths in scripts/round2/{wala,sootup,opal}/cp.txt,
# probe classes compiled into $SC/{soot,wala,sootup}-probe-classes, Java fixtures compiled
# with `javac --release 8` into fixtures/r2-java/<v>-classes.
set -u
PR=<probe>
SC=${SC:?set SC to the directory holding the compiled probe classes}
L="flock -w 560 $PR/heavy.lock timeout 240"
FX=$PR/fixtures/r2-py
FJ=$PR/fixtures/r2-java
O=$PR/out/round2
cd "$O"

# PyCG (vitsalis/PyCG 8d5dc40837803beef1d8d379fbf2cdad6cd94641)
PY=$PR/third_party/pycg/venv/bin/python
for f in fixture_i fixture_ii; do
  $L $PY -I -m pycg --package $FX $FX/$f.py -o $O/pycg-$f.json
  $L $PY -I -W ignore -m pycg --package $FX --fasten --product probe --forge local \
    --version 0 --timestamp 0 $FX/$f.py -o $O/pycg-fasten-$f.json
done

# Jarvis (pythonJaRvis/pythonJaRvis.github.io 8871b86494a1aec0095686ffa543994a7361af88)
JV=$PR/third_party/jarvis/venv/bin/python
JC=$PR/third_party/jarvis/src/Jarvis/tool/Jarvis/jarvis_cli.py
for f in fixture_i fixture_ii; do
  $L $JV -I $JC $FX/$f.py --package $FX -o $O/jarvis-$f.json
  $L $JV -I -W ignore $JC $FX/$f.py --package $FX --precision -o $O/jarvis-precision-$f.json
  $L $JV -I -W ignore $PR/scripts/round2/jarvis_probe.py $FX/$f.py $FX | sed 1d > $O/jarvis-probe-$f.json
done

# Soot 4.7.1
SJ=$PR/third_party/soot-jar/soot-4.7.1-jar-with-dependencies.jar
soot() { $L java -Xmx1g -cp $SJ:$SC/soot-probe-classes SootProbe "$@" 2>&1 | grep -v '^SLF4J'; }
soot $FJ/i-classes cha > $O/soot-cha-fixture_i.txt
soot $FJ/i-classes spark > $O/soot-spark-fixture_i.txt
soot $FJ/ii-classes cha all-reachable > $O/soot-cha-fixture_ii.txt
soot $FJ/ii-classes spark all-reachable > $O/soot-spark-fixture_ii.txt
soot $FJ/iii-classes cha > $O/soot-cha-fixture_iii.txt
soot $FJ/iv-classes cha > $O/soot-cha-fixture_iv.txt

# WALA 1.8.0
WCP=$(cat $PR/scripts/round2/wala/cp.txt)
wala() { $L java -Xmx1g -cp $WCP:$SC/wala-probe-classes WalaProbe "$@" 2>&1; }
wala $FJ/i-classes $PR/scripts/round2/wala/exclusions.txt cha main > $O/wala-cha-fixture_i.txt
wala $FJ/i-classes $PR/scripts/round2/wala/exclusions-0cfa.txt 0cfa main > $O/wala-0cfa-fixture_i.txt
wala $FJ/ii-classes $PR/scripts/round2/wala/exclusions.txt cha all > $O/wala-cha-fixture_ii.txt
wala $FJ/ii-classes $PR/scripts/round2/wala/exclusions-0cfa.txt 0cfa all > $O/wala-0cfa-fixture_ii.txt
wala $FJ/iii-classes $PR/scripts/round2/wala/exclusions.txt cha main > $O/wala-cha-fixture_iii.txt
wala $FJ/iv-classes $PR/scripts/round2/wala/exclusions.txt cha main > $O/wala-cha-fixture_iv.txt

# SootUp 3.0.1
UCP=$(cat $PR/scripts/round2/sootup/cp.txt)
sootup() { $L java -Xmx1g -cp $UCP:$SC/sootup-probe-classes SootUpProbe "$@" 2>/dev/null; }
sootup $FJ/i-classes cha main > $O/sootup-cha-fixture_i.txt
sootup $FJ/i-classes rta main > $O/sootup-rta-fixture_i.txt
sootup $FJ/ii-classes cha speak > $O/sootup-cha-fixture_ii.txt
sootup $FJ/ii-classes rta speak > $O/sootup-rta-fixture_ii.txt
sootup $FJ/iii-classes cha main > $O/sootup-cha-fixture_iii.txt
sootup $FJ/iv-classes cha main > $O/sootup-cha-fixture_iv.txt

# OPAL 7.0.0 (tools_3). --mainClass and --entryPoints both fail in this release (see report);
# the default application entry-point finder and --library mode work.
OCP=$(cat $PR/scripts/round2/opal/cp.txt)
opal() { out=$1; shift; $L java -Xmx1g -Dorg.opalj.threads.CPUBoundTasks=1 \
  -Dorg.opalj.threads.IOBoundTasks=1 -cp $OCP org.opalj.support.info.CallGraph "$@" \
  --result $O/$out.jcg.json -t 0 --noLogs > $O/$out.log 2>&1; }
opal opal-cha-fixture_i --cp $FJ/i-classes --noJDK --callGraph CHA --callers sound --callees speak
opal opal-pointsto-fixture_i --cp $FJ/i-classes --noJDK --callGraph PointsTo --callers sound
opal opal-cha-fixture_ii --cp $FJ/ii-classes --noJDK --callGraph CHA --library --callers sound
opal opal-pointsto-fixture_ii --cp $FJ/ii-classes --noJDK --callGraph PointsTo --library --callers sound
opal opal-cha-fixture_iii --cp $FJ/iii-classes --noJDK --callGraph CHA --callers sound
opal opal-cha-fixture_iv --cp $FJ/iv-classes --noJDK --callGraph CHA --callers sound
