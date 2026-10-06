#!/usr/bin/env python3
"""Opt-in real-machine acceptance tests. Requires two already enrolled accounts.

python3 scripts/e2e.py --on green-lighthouse --origin macbook
Add --providers to make real Codex requests in both directions.
Add --claude only when Claude is installed and signed in on the destination.
No enrollment, key distribution, service installation, or provider login changes.
Only generated test files and uniquely named temporary capabilities are used.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--mesh', default=str(Path.home()/'.local/bin/mesh'))
p.add_argument('--on', required=True)
p.add_argument('--origin', required=True)
p.add_argument('--providers', action='store_true', help='Test Codex in both directions (uses existing logins)')
p.add_argument('--claude', action='store_true', help='Also test Claude on the destination; requires its own login')
p.add_argument('--case', action='append', help='Run only a named test (repeatable)')
p.add_argument('--output', default='artifacts/e2e')
args = p.parse_args()
run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
root = (Path(args.output)/run_id).resolve()
root.mkdir(parents=True)
results = []
terminal = {'completed','failed','cancelled','needs_attention'}


def cli(*argv, check=True, env=None):
    proc = subprocess.run([args.mesh,*map(str,argv)],capture_output=True,text=True,timeout=75,env=env)
    if check and proc.returncode:
        raise AssertionError(f'mesh {shlex.join(list(map(str,argv)))}: {proc.stderr.strip()}')
    return proc


def data(*argv):
    return json.loads(cli(*argv).stdout)


def submit(name, command, *, target=None, extra=(), env=None):
    proc=cli('run','--on',target or args.on,'--agent','shell','--output',root/name,*extra,'--',*command,env=env)
    return json.loads(proc.stdout)['id']


def wait(job, desired=None, timeout=120):
    until=time.monotonic()+timeout
    while time.monotonic()<until:
        value=data('status',job)
        if value['state'] in terminal:
            if desired: assert value['state']==desired, value
            return value
        time.sleep(.35)
    # Cancel only this test's job and children recorded on the origin. This
    # harness uses one nested hop; production cancellation remains explicit.
    value=data('status',job)
    cli('cancel',job,check=False)
    for child in value.get('children',[]):
        cli('cancel',child,check=False)
    raise AssertionError(f'timed out waiting for {job}; requested test-job cancellation')


def case(name, fn):
    if args.case and name not in args.case: return
    started=time.monotonic()
    try:
        details=fn() or {}
        row={'name':name,'status':'pass',**details}
    except Exception as e:
        row={'name':name,'status':'fail','error':str(e)}
    row['seconds']=round(time.monotonic()-started,2)
    results.append(row)
    print(f"{row['status'].upper():7} {name}"+(f" — {row['error']}" if 'error' in row else ''),flush=True)
    (root/'report.json').write_text(json.dumps({'run':run_id,'target':args.on,'origin':args.origin,'results':results},indent=2)+'\n')


def discovery():
    value=data('discover','--json')
    assert any(args.on in x['name'] or args.on in x['host'] for x in value['candidates'])
    assert not any(x['name']=='funnel-ingress-node' for x in value['candidates'])
    return {'target_found':True,'tailscale_infrastructure_filtered':True}


def inventory():
    rows=data('machines','--check','--json')
    for name in [args.on,args.origin]:
        row=next(x for x in rows if x['machine']['name']==name)
        assert row['reachable'] and row['inventory']['os'], row
    return {'reachable':[args.origin,args.on]}


def transfer():
    source=root/"input with spaces ' café.txt"
    source.write_text('A real round trip. Unicode: café. Literal: $(not a command).\n')
    job=submit('transfer',['sh','-c','hostname > outputs/host.txt; cp '+shlex.quote('inputs/'+source.name)+' outputs/echo.txt; printf LIVE_TRACE_OK'],extra=['--input',source,'--expect','echo.txt'])
    status=wait(job,'completed')
    assert status['delivery'] in ('pending','delivered')
    trace=cli('watch',job).stdout
    assert 'LIVE_TRACE_OK' in trace
    cli('collect',job)
    assert (root/'transfer/echo.txt').read_bytes()==source.read_bytes()
    assert data('status',job)['delivery']=='delivered'
    cli('collect',job)
    return {'job':job,'execution_before_collection':'completed','delivery_before_collection':status['delivery'],'delivery_after_collection':'delivered','sha256':hashlib.sha256(source.read_bytes()).hexdigest()}


def reverse():
    # The command is executed on the server, which submits a child to the Mac.
    script='mesh run --on '+shlex.quote(args.origin)+' --agent shell --output "$MESH_OUTPUT_DIR/from-mac" --expect reverse.txt --wait -- sh -c '+shlex.quote('hostname > outputs/reverse.txt')
    job=submit('reverse',['sh','-c',script])
    wait(job,'completed');cli('collect',job)
    hostname=(root/'reverse/from-mac/reverse.txt').read_text().strip()
    assert hostname and args.on not in hostname
    return {'parent_job':job,'child_hostname':hostname,'path':'server → Mac → server → Mac'}


def placement():
    job=submit('placement',['sh','-c','mesh run --on '+shlex.quote(args.origin)+' --agent shell -- true'],extra=['--only'])
    wait(job,'failed')
    trace=cli('watch',job,check=False).stdout
    assert 'delegation' in trace or 'depth' in trace or 'placement' in trace,trace
    return {'job':job,'forbidden_child_rejected':True}


def cancellation():
    job=submit('cancel',['sh','-c','echo STARTED; sleep 120; echo SHOULD_NOT_EXIST > outputs/late.txt'])
    for _ in range(30):
        if data('status',job)['state']=='running': break
        time.sleep(.2)
    cli('cancel',job);wait(job,'cancelled')
    return {'job':job}


def failed_command():
    job=submit('failed',['sh','-c','echo EXPECTED_FAILURE >&2; exit 23'])
    value=wait(job,'failed');assert value['exit_code']==23
    return {'job':job,'exit_code':23}


def required_output():
    job=submit('missing',['true'],extra=['--expect','report.pdf'])
    status=wait(job,'failed');assert 'report.pdf' in status['error']
    return {'job':job,'missing_output_detected':True}


def no_overwrite():
    destination=root/'conflict';destination.mkdir();(destination/'file.txt').write_text('original local content')
    job=submit('conflict',['sh','-c','printf different > outputs/file.txt'])
    wait(job,'completed');assert cli('collect',job,check=False).returncode
    assert (destination/'file.txt').read_text()=='original local content'
    assert data('status',job)['delivery']=='pending'
    cli('collect',job,'--output',root/'conflict-recovered')
    assert (root/'conflict-recovered/file.txt').read_text()=='different'
    return {'job':job,'existing_file_preserved':True}


def parallel():
    def one(i):
        job=submit(f'parallel-{i}',['sh','-c',f'sleep 2; echo {i} > outputs/result.txt'])
        wait(job,'completed');cli('collect',job)
        assert (root/f'parallel-{i}/result.txt').read_text().strip()==str(i)
        return job
    with ThreadPoolExecutor(max_workers=4) as pool: jobs=list(pool.map(one,range(4)))
    return {'jobs':jobs,'simultaneous_submissions':4}


def lost_reply():
    wrapper=root/'fault-injection';wrapper.mkdir()
    real_ssh=shutil.which('ssh')
    # Real SSH still executes remotely; discard exactly the submit reply.
    script='''#!/usr/bin/env python3
import json,os,subprocess,sys
payload=sys.stdin.buffer.read()
p=subprocess.run([REAL_SSH,*sys.argv[1:]],input=payload,stdout=subprocess.PIPE)
try: is_submit=json.loads(payload).get('action')=='submit'
except Exception: is_submit=False
if is_submit and p.returncode==0: sys.exit(255)
sys.stdout.buffer.write(p.stdout);sys.exit(p.returncode)
'''.replace('REAL_SSH',repr(real_ssh))
    (wrapper/'ssh').write_text(script);(wrapper/'ssh').chmod(0o700)
    env=dict(os.environ,PATH=str(wrapper)+os.pathsep+os.environ['PATH'])
    proc=cli('run','--on',args.on,'--agent','shell','--output',root/'retry','--','sh','-c','printf x >> outputs/executions.txt',check=False,env=env)
    assert proc.returncode,proc.stdout
    matches=re.findall(r'\b[0-9a-f]{32}\b',proc.stderr)
    assert matches,proc.stderr
    job=matches[0]
    cli('retry',job);wait(job,'completed');cli('retry',job);cli('collect',job)
    assert (root/'retry/executions.txt').read_text()=='x'
    return {'job':job,'remote_execution_count':1,'lost_submit_reply_recovered':True}


def capability():
    name='e2e-'+run_id.lower()
    job=submit('capability',['mesh','capability','add',name,'--environment','test-only','--note','Verified by an explicit Mesh acceptance test.'])
    wait(job,'completed');cli('sync')
    inv=data('machine','show',args.on)
    assert any(x['name']==name for x in inv['capabilities'].values())
    job=submit('capability-remove',['mesh','capability','remove',name,'--environment','test-only'])
    wait(job,'completed');cli('sync')
    assert not any(x['name']==name for x in data('machine','show',args.on)['capabilities'].values())
    return {'shared_observation_added_and_removed':True}


def automatic_delivery():
    job=submit('automatic-delivery',['sh','-c','hostname > outputs/delivered.txt'])
    wait(job,'completed')
    until=time.monotonic()+65
    while time.monotonic()<until:
        if data('status',job)['delivery']=='delivered':
            assert args.on in (root/'automatic-delivery/delivered.txt').read_text()
            return {'job':job,'delivered_without_collect':True}
        time.sleep(1)
    raise AssertionError('No automatic delivery within 65 seconds; ensure mesh service is running on the submitting machine.')


def provider(agent):
    brief=root/'handoff.md';brief.write_text('Required context marker: HANDOFF_CONTEXT_42. Include it verbatim in your output.\n')
    source=root/'instructions.md';source.write_text('Read this file, and write outputs/report.txt with REMOTE_AGENT_42 and the marker supplied in the handoff context.\n')
    proc=cli('run','--on',args.on,'--agent',agent,'--input',source,'--context',brief,'--output',root/agent,'--expect','report.txt','--only','Read inputs/instructions.md and follow its instructions. Use your edit tool to create the output. Do not inspect files outside this task workspace.')
    job=json.loads(proc.stdout)['id'];status=wait(job,timeout=180)
    if status['state']=='needs_attention':
        return {'status':'blocked','job':job,'error_code':status.get('error_code'),'reason':status.get('error')}
    assert status['state']=='completed',status
    cli('collect',job);text=(root/agent/'report.txt').read_text()
    assert 'HANDOFF_CONTEXT_42' in text and 'REMOTE_AGENT_42' in text
    return {'job':job,'input_and_context_verified':True}


def reverse_codex():
    prompt='Use your edit tool to create outputs/report.txt containing exactly MAC_CODEX_CHILD_42. This is a small Mesh test. Do not inspect files outside this task workspace.'
    script='mesh run --on '+shlex.quote(args.origin)+' --agent codex --only --expect report.txt --output "$MESH_OUTPUT_DIR/from-origin" --wait '+shlex.quote(prompt)
    job=submit('reverse-codex',['sh','-c',script],extra=['--expect','from-origin/report.txt'])
    status=wait(job,'completed',timeout=240)
    cli('collect',job)
    assert (root/'reverse-codex/from-origin/report.txt').read_text().strip()=='MAC_CODEX_CHILD_42'
    return {'job':job,'child_jobs':status.get('children',[]),'real_agent_on_origin':True}


for name,fn in [('discovery',discovery),('live inventory',inventory),('files, traces, execution vs delivery',transfer),('reverse work and nested delegation',reverse),('strict placement',placement),('cancellation',cancellation),('failed command',failed_command),('required deliverable',required_output),('overwrite protection',no_overwrite),('parallel jobs and queue',parallel),('lost SSH reply, retry without duplicates',lost_reply),('shared capabilities',capability),('automatic delivery',automatic_delivery)]:
    case(name,fn)
if args.providers:
    case('real codex',lambda:provider('codex'))
    case('reverse codex',reverse_codex)
if args.claude:
    case('real claude',lambda:provider('claude'))
print(f'\nReport: {root}/report.json',flush=True)
raise SystemExit(1 if any(x['status']=='fail' for x in results) else 0)
