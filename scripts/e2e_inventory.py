#!/usr/bin/env python3
"""Verify change-driven inventory on two already-enrolled computers.

Example: python3 scripts/e2e_inventory.py --ssh blas@green-lighthouse
Uses real SSH, edits only uniquely named test capabilities, and restarts the
installed Mesh services to verify that acknowledgments survive a restart.
"""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import shlex
import subprocess
import time
import uuid

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--ssh', required=True, help='Authorized SSH user@host for the other computer')
p.add_argument('--idle-seconds', type=int, default=65)
p.add_argument('--output', default='artifacts/e2e/inventory-events.json')
args = p.parse_args()
mesh = str(Path.home()/'.local/bin/mesh')
marker = 'inventory-test-' + uuid.uuid4().hex[:12]

reader = '''from pathlib import Path
import json
s=Path.home()/'.ai-mesh'
c=json.loads((s/'config.json').read_text())
print(json.dumps({'name':c['self']['name'],'id':c['self']['id'],
 'inventories':{p.stem:json.loads(p.read_text()) for p in (s/'machines').glob('*.json')},
 'contacts':{p.stem:json.loads(p.read_text()) for p in (s/'contacts').glob('*.json')},
 'service':json.loads((s/'daemon-status.json').read_text())}))
'''
editor = '''from pathlib import Path
from datetime import datetime, timezone
import json,os,tempfile
s=Path.home()/'.ai-mesh'
c=json.loads((s/'config.json').read_text())
p=s/'machines'/(c['self']['id']+'.json')
x=json.loads(p.read_text())
name=MARKER
if REMOVE:
 x['capabilities'].pop(name,None)
else:
 x['capabilities'][name]={'name':name,'note':'Temporary direct file-edit publication test','updated_at':datetime.now(timezone.utc).isoformat()}
# Leave the revision unchanged; Mesh must normalize a direct editor save.
fd,tmp=tempfile.mkstemp(dir=p.parent,prefix='.inventory-test-')
with os.fdopen(fd,'w') as f: json.dump(x,f)
os.replace(tmp,p)
'''


def python(remote, code):
    if remote:
        proc = subprocess.run(['ssh','-o','BatchMode=yes',args.ssh,'python3 -c '+shlex.quote(code)],capture_output=True,text=True,timeout=20)
    else:
        proc = subprocess.run(['python3','-c',code],capture_output=True,text=True,timeout=20)
    assert proc.returncode == 0, proc.stderr
    return proc.stdout


def snapshot(remote):
    return json.loads(python(remote,reader))


def edit(remote, remove=False):
    python(remote,editor.replace('MARKER',repr(marker)).replace('REMOVE',repr(remove)))


def await_copy(destination_remote, source_id, present):
    start=time.monotonic()
    while time.monotonic()-start < 10:
        snap=snapshot(destination_remote)
        inv=snap['inventories'][source_id]
        if (marker in inv['capabilities']) == present:
            return round(time.monotonic()-start,3)
        time.sleep(.15)
    raise AssertionError('File update was not propagated within 10 seconds')


report={'tested_at':datetime.now(timezone.utc).isoformat(),'results':[]}
def record(name, **details):
    report['results'].append({'name':name,'status':'pass',**details})
    print('PASS',name,details,flush=True)


local,remote=snapshot(False),snapshot(True)
assert local['id'] in remote['inventories'] and remote['id'] in local['inventories']
try:
    edit(False)
    seconds=await_copy(True,local['id'],True)
    record('direct Mac file edit published',seconds=seconds)
    edit(True)
    seconds=await_copy(False,remote['id'],True)
    record('direct server file edit published',seconds=seconds)
    edit(False,True);edit(True,True)
    await_copy(True,local['id'],False);await_copy(False,remote['id'],False)
    record('test capability removals propagated')
    # Reading cached files over SSH does not issue inventory or info RPCs.
    before=[snapshot(False),snapshot(True)]
    print('Observing idle services for',args.idle_seconds,'seconds...',flush=True)
    time.sleep(args.idle_seconds)
    after=[snapshot(False),snapshot(True)]
    for a,b in zip(before,after):
        assert a['contacts']==b['contacts'], (a['name'],'received an unchanged description')
        assert a['service']['last_tick']!=b['service']['last_tick'], (a['name'],'service did not tick')
    record('idle maintenance ticks exchange no specs',seconds=args.idle_seconds)
    subprocess.run([mesh,'service','install'],check=True,capture_output=True,timeout=20)
    subprocess.run(['ssh','-o','BatchMode=yes',args.ssh,'~/.local/bin/mesh service install'],check=True,capture_output=True,timeout=20)
    until=time.monotonic()+15
    while True:
        restarted=[snapshot(False),snapshot(True)]
        if all(a['service']['pid']!=b['service']['pid'] for a,b in zip(after,restarted)):
            break
        assert time.monotonic()<until,'Services did not restart'
        time.sleep(.2)
    for a,b in zip(after,restarted):
        assert a['contacts']==b['contacts'], (a['name'],'resent specs after restart')
    record('service restarts retain inventory acknowledgments')
    report['status']='pass'
except Exception as error:
    report['status']='fail'
    report['error']=str(error)
    raise
finally:
    # Remove only this run's markers, including if an assertion failed.
    edit(False,True);edit(True,True)
    output=Path(args.output)
    output.parent.mkdir(parents=True,exist_ok=True)
    output.write_text(json.dumps(report,indent=2)+'\n')
print('Report:',args.output,flush=True)
