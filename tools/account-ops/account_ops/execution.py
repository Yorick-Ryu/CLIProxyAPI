"""Isolated acquisition and resumable post-login steps over guarded CPA writes."""
import json
import contextlib
import fcntl
import stat
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time
from .cpa import Management
from .policy import automatic,AUTH_ERROR,TRANSIENT,periodic_decision
from .security import OpsError,lock_email,private_file
from .store import PHASES,REASONS


class Executor:
    def __init__(self,store,config):
        allowed={'origin','management_key_file','login_project','python','proxy_helper','notification_helper','enrollment_root'}
        if set(config)-allowed:raise OpsError('invalid_input')
        self.store,self.config=store,config
        for key in ('python','login_project'):
            if not Path(config[key]).is_absolute() or not Path(config[key]).exists():raise OpsError('unavailable',503)
        self.management=Management(config['origin'],config['management_key_file'])
        self.capabilities={'proxy':bool(config.get('proxy_helper')),'human_takeover':False,'synthetic':False}
        self.revisions={};self.processes={};self.process_mutex=threading.RLock()
        self.inventory()

    @contextlib.contextmanager
    def exclusive(self,job):
        if self.store.instance!='clouvider' or not job.get('account_id') or job['kind'] in ('record','batch'):
            yield;return
        files=[]
        try:
            for path in (Path('/root/cpa-auto-reauth/watch.lock'),Path('/run/clouvider-cpa-inspection.lock')):
                if not path.parent.exists():raise OpsError('unavailable',503)
                fd=os.open(path,os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
                stream=os.fdopen(fd,'r+b');files.append(stream)
                if not stat.S_ISREG(os.fstat(fd).st_mode):raise OpsError('unavailable',503)
                try:fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
                except BlockingIOError:raise OpsError('locked',409) from None
            state_path=Path('/root/cpa-auto-reauth/state.json')
            if state_path.exists():
                state=json.loads(private_file(state_path))
                with self.store.mutex:account=self.store.db.execute('SELECT identity FROM accounts WHERE id=?',(job['account_id'],)).fetchone()
                for address,record in state.get('accounts',{}).items():
                    if lock_email(address)!=account['identity']:continue
                    if record.get('job') and (Path(record['job'])/'result.json').exists():continue
                    pid=record.get('pid')
                    if pid:
                        try:os.kill(int(pid),0)
                        except (ProcessLookupError,ValueError):continue
                        raise OpsError('locked',409)
            yield
        finally:
            for stream in reversed(files):stream.close()

    def enroll_automatic(self,job):
        if self.store.instance!='clouvider' or not job['snapshot']['preferences']['auto_enroll']:return
        target=self.target(job,True)
        if str(target.get('plan_type','')).lower()=='free':return
        root=Path(self.config.get('enrollment_root','/root/cpa-auto-reauth'))
        if root!=Path('/root/cpa-auto-reauth') or not root.exists():raise OpsError('unavailable',503)
        with self.store.mutex:record=self.store.db.execute('SELECT version FROM credentials WHERE account_id=?',(job['account_id'],)).fetchone()
        if not record:raise OpsError('missing_credentials')
        secret=self.store.credentials(job['account_id'],record['version'])
        path=root/'credentials.json'
        with (root/'credentials.lock').open('a') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            existing=json.loads(private_file(path)) if path.exists() else {}
            backup=root/'backups';backup.mkdir(mode=0o700,exist_ok=True)
            if path.exists() and not (backup/(job['id']+'.json')).exists():
                fd=os.open(backup/(job['id']+'.json'),os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
                with os.fdopen(fd,'wb') as stream:stream.write(private_file(path))
            existing[lock_email(target['email'])]={k:secret[k] for k in ('password','totp')}
            temporary=path.with_name('.credentials-'+job['id']+'.tmp')
            fd=os.open(temporary,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
            with os.fdopen(fd,'w') as stream:
                json.dump(existing,stream);stream.flush();os.fsync(stream.fileno())
            os.replace(temporary,path)
            if json.loads(private_file(path)).get(lock_email(target['email']))!=existing[lock_email(target['email'])]:raise OpsError('configuration_changed')

    def inventory(self):
        targets=self.management.targets()
        for target in targets:
            with self.store.mutex:
                pending=self.store.db.execute("SELECT a.id FROM accounts a JOIN jobs j ON a.id=j.account_id WHERE a.identity=? AND a.workspace='' AND j.kind='login' AND j.state NOT IN ('succeeded','partial','failed','cancelled','skipped')",(lock_email(target['email']),)).fetchone()
            if pending: continue
            account_id=self.store.account(target['email'],target['workspace'],target['auth_ref'])
            with self.store.transaction():
                self.store.db.execute('UPDATE accounts SET auth_ref=? WHERE id=?',(target['auth_ref'],account_id))
        return targets

    def snapshot(self,account_id,kind):
        with self.store.mutex:account=self.store.db.execute('SELECT * FROM accounts WHERE id=?',(account_id,)).fetchone()
        if not account:raise OpsError('invalid_input')
        matches=[target for target in self.management.targets() if lock_email(target['email'])==account['identity']]
        if kind=='login':
            if matches:raise OpsError('ambiguous_account',409)
            return {'target':None}
        matching=[target for target in matches if target['workspace']==account['workspace']]
        if len(matching)!=1:raise OpsError('ambiguous_account',409)
        return {'target':matching[0]}

    def target(self,job,allow_new=False):
        with self.store.mutex:account=self.store.db.execute('SELECT * FROM accounts WHERE id=?',(job['account_id'],)).fetchone()
        expected=job['snapshot'].get('target')
        matches=[target for target in self.management.targets() if lock_email(target['email'])==account['identity']]
        if expected:matches=[target for target in matches if target['id']==expected['id']]
        if not expected and not allow_new:
            if matches:raise OpsError('configuration_changed',409)
            return None
        if len(matches)!=1:raise OpsError('ambiguous_account',409)
        target=matches[0]
        version=self.revisions.get(job['id'],expected['config_version'] if expected else None)
        if version and target['config_version']!=version:raise OpsError('configuration_changed',409)
        return target

    def guard(self,job):
        if not job.get('account_id'):return
        target=self.target(job,job['id'] in self.revisions)
        if job['source']=='auto_reauth':
            if self.store.instance!='clouvider':raise OpsError('invalid_input')
            row=next((row for row in self.management.files() if target and row['name']==target['auth_ref']),{})
            full=self.management.auth(target['auth_ref']) if target else {}
            automatic(row|{'note':full.get('note'),'plan_type':full.get('plan_type')},self.store.clock(),require_failure=job['id'] not in self.revisions)

    def perform_login(self,job,report,check):
        private=self.store.root/'jobs'/job['id'];private.mkdir(mode=0o700,parents=True,exist_ok=True)
        target=job['snapshot'].get('target')
        with self.store.mutex:account=self.store.db.execute('SELECT * FROM accounts WHERE id=?',(job['account_id'],)).fetchone()
        if 'draft_id' in job['snapshot']:
            secret=self.store.private_job_input(job['id'])
        elif job['credential_version'] is not None:secret=self.store.credentials(job['account_id'],job['credential_version'])
        else:raise OpsError('missing_credentials')
        request={'job_id':job['id'],'email':account['email'],'credentials':secret,'private_dir':str(private),'origin':self.config['origin'],
                 'management_key_file':self.config['management_key_file'],'login_project':self.config['login_project'],
                 'target':target,'automatic':job['source']=='auto_reauth'}
        environment=os.environ.copy();environment['PYTHONPATH']=str(Path(__file__).resolve().parents[1]);environment['PYTHONUNBUFFERED']='1'
        with (private/'process.log').open('ab') as errors:
            process=subprocess.Popen([self.config['python'],'-m','account_ops.browser_child'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=errors,env=environment,start_new_session=True)
            from .processes import remember
            try:remember(private/'browser-owner.json',process.pid)
            except BaseException:
                process.terminate();process.wait();raise
            with self.process_mutex:self.processes[job['id']]=process
            cancelled=threading.Event()
            def control():
                while not cancelled.wait(1):
                    if self.store.get_job(job['id'])['cancel_requested']:
                        (private/'cancel').touch(mode=0o600)
            controller=threading.Thread(target=control,daemon=True);controller.start()
            succeeded=False;failure='failed'
            try:
                process.stdin.write(json.dumps(request).encode());process.stdin.close()
                # The child's ordinary stdout/stderr are redirected before loading any
                # browser code. This pipe accepts only the bounded event protocol.
                for line in process.stdout:
                    if len(line)>4096:raise OpsError('failed')
                    message=json.loads(line)
                    phase,reason=message.get('phase'),message.get('reason')
                    if phase not in PHASES or reason not in REASONS:raise OpsError('failed')
                    report(phase,reason)
                    if reason=='success':succeeded=True
                    elif reason not in ('running','human_required'):failure=reason
                process.wait()
            finally:
                cancelled.set();controller.join()
                if process.poll() is None:
                    # Only the process group created for this job is owned here.
                    os.killpg(process.pid,signal.SIGTERM)
                    try:process.wait(timeout=10)
                    except subprocess.TimeoutExpired:os.killpg(process.pid,signal.SIGKILL);process.wait()
                with self.process_mutex:self.processes.pop(job['id'],None)
            if self.store.get_job(job['id'])['cancel_requested']:raise OpsError('cancelled')
            if not succeeded:raise OpsError(failure)
        for attempt in range(11):
            try:
                actual=self.target(job,allow_new=True)
                auth=self.management.auth(actual['auth_ref'])
                if target and (auth.get('account_id')!=target['workspace'] or auth.get('plan_type')!=target['plan_type']):raise OpsError('workspace_mismatch')
                if auth.get('access_token') and auth.get('account_id'):
                    self.revisions[job['id']]=actual['config_version']
                    with self.store.transaction():
                        self.store.db.execute('UPDATE accounts SET workspace=?,auth_ref=? WHERE id=?',(actual['workspace'],actual['auth_ref'],job['account_id']))
                    return
            except OpsError as exc:
                if exc.reason not in ('ambiguous_account',):raise
            if attempt<10:time.sleep(2)
        raise OpsError('failed')

    def step(self,job,phase):
        if phase=='proxy':
            if job['snapshot']['preferences']['proxy_policy']=='preserve':return
            target=self.target(job,True);auth=self.management.auth(target['auth_ref'])
            if auth.get('proxy_url') or auth.get('proxy-url'):return
            helper=self.config.get('proxy_helper')
            if not helper:raise OpsError('unavailable',503)
            # The helper is an operator-installed fixed executable, never a client
            # command. It must hold the existing allocation lock through readback.
            result=self._helper(helper,{'instance':self.store.instance,'target_id':target['id'],'auth_ref':target['auth_ref'],'config_version':target['config_version']})
            if result.get('verified') is not True:raise OpsError('unavailable',503)
            self.revisions[job['id']]=result['config_version']
        if phase=='preferences':
            target=self.target(job,True)
            auth=self.management.auth(target['auth_ref'])
            desired=job['snapshot']['preferences'];fields={}
            end=job['snapshot'].get('order',{}).get('expires_at')
            if job['kind']=='login' and end:
                import datetime as dt
                from .policy import TZ
                stamp=dt.datetime.fromisoformat(end).astimezone(TZ).strftime('%Y-%m-%d %H:%M')
                note='到期时间：'+stamp+'（北京时间）'
                if auth.get('note') not in (None,'',note):raise OpsError('configuration_changed',409)
                if auth.get('note')!=note:fields['note']=note
            if job['kind']=='login' and auth.get('websockets')!=desired['websockets']:fields['websockets']=desired['websockets']
            if job['kind']=='apply_preferences' and auth.get('websockets')!=desired['websockets']:fields['websockets']=desired['websockets']
            # Priority is applied only after the engine records a passed or explicit
            # user-skipped evaluation. The early preference step cannot raise it.
            if self.store.get_job(job['id'])['phase']=='preferences' and self._quality_authorized(job):
                _,priority=self.priorities(job)
                if priority>int(auth.get('priority') or 0):fields['priority']=priority
            if fields:self.revisions[job['id']]=self.management.patch(target,fields)

    def _quality_authorized(self,job):
        events=self.store.records(job_id=job['id'],limit=200)['items']
        return any(event['phase']=='quality' and event['reason'] in ('quality_passed','user_skipped') for event in events)

    def priorities(self,job):
        target=self.target(job,True);auth=self.management.auth(target['auth_ref']);current=int(auth.get('priority') or 0)
        # Reauthorization retains the existing priority. New and explicit preference
        # jobs may raise it to the frozen target, never lower a higher value.
        return current,current if job['kind']=='reauth' else max(current,job['snapshot']['preferences']['priority'])

    def quality(self,job):
        target=self.target(job,True)
        return self.management.candy(target['auth_ref'],lambda:self.guard(job))

    def verify(self,job):
        target=self.target(job,True);auth=self.management.auth(target['auth_ref'])
        if not auth.get('access_token') or auth.get('account_id')!=target['workspace']:raise OpsError('workspace_mismatch')

    def scheduled(self,job,check):
        from .scheduling import legacy_present
        if self.store.instance!='clouvider':raise OpsError('invalid_input')
        if legacy_present(job['kind']):raise OpsError('conflict',409)
        if job['kind']=='quality_inspection' and job.get('account_id'):
            self.periodic(job,check);return
        targets=self.inventory()
        rows={row['name']:row for row in self.management.files()}
        for target in targets:
            check()
            with self.store.mutex:
                account=self.store.db.execute('SELECT id FROM accounts WHERE instance=? AND identity=? AND workspace=?',(self.store.instance,lock_email(target['email']),target['workspace'])).fetchone()
            if not account:continue
            account_id=account['id'];row=rows.get(target['auth_ref'],{})
            if job['kind']=='health_scan':
                self.health_observation(job,account_id,row);continue
            if job['kind']=='quality_inspection':
                import fnmatch
                plan=str(target.get('plan_type') or '').lower()
                if target.get('disabled') or row.get('unavailable') or row.get('status')!='active' or not ('business' in plan or plan=='team'):continue
                if not any(fnmatch.fnmatchcase(value,pattern) for value in (target['auth_ref'],target['email']) for pattern in ('codex-*','cdk-*')):continue
                self.store.submit('quality_inspection',account_id,'periodic:'+job['id']+':'+account_id,source='scheduled',extra={'target':target})
                continue
            if job['kind']=='auto_reauth_scan':
                self.auto_candidate(job,account_id,target,row)

    def health_observation(self,job,account_id,row):
        bad=bool(not row or row.get('status') not in ('active','disabled') or row.get('unavailable')) and not row.get('disabled')
        with self.store.transaction():
            prior=self.store.db.execute('SELECT * FROM health_state WHERE account_id=?',(account_id,)).fetchone()
            changed=prior is not None and bool(prior['bad'])!=bad
            if prior is None or changed:
                self.store.db.execute('INSERT OR REPLACE INTO health_state VALUES(?,?,?)',(account_id,int(bad),job['id']))
                self.store._event(job['id'],'preflight','failed' if bad else 'healthy','health','scheduled',account_id)
        if changed or (prior is None and bad):self.store.queue_notification(job['id'],'failed' if bad else 'healthy')

    def auto_candidate(self,job,account_id,target,row):
        message=str(row.get('status_message') or '')
        if row.get('status')=='active' and not row.get('unavailable'):
            with self.store.transaction():self.store.db.execute('DELETE FROM recovery_state WHERE account_id=?',(account_id,))
            return
        if TRANSIENT.search(message) or not AUTH_ERROR.search(message):return
        with self.store.mutex:
            credential=self.store.db.execute('SELECT version FROM credentials WHERE account_id=?',(account_id,)).fetchone()
            recovery=self.store.db.execute('SELECT * FROM recovery_state WHERE account_id=?',(account_id,)).fetchone()
        if not credential:return
        full=self.management.auth(target['auth_ref'])
        try:automatic(row|{'note':full.get('note'),'plan_type':full.get('plan_type')},self.store.clock())
        except OpsError:return
        if recovery:
            if recovery['blocked'] or recovery['attempts']>=2 or recovery['next_retry']>self.store.clock():return
            if recovery['job_id']:
                prior=self.store.get_job(recovery['job_id'])
                if prior['state'] in ('waiting_human','cancelled'):
                    with self.store.transaction():self.store.db.execute('UPDATE recovery_state SET blocked=1 WHERE account_id=?',(account_id,))
                    return
                if prior['state'] not in ('succeeded','partial','failed','cancelled','skipped'):return
                if prior['updated']+900>self.store.clock():return
        else:
            from .store import uid
            with self.store.transaction():self.store.db.execute('INSERT INTO recovery_state(account_id,generation) VALUES(?,?)',(account_id,uid()))
            with self.store.mutex:recovery=self.store.db.execute('SELECT * FROM recovery_state WHERE account_id=?',(account_id,)).fetchone()
        attempt=recovery['attempts']+1
        recovery_job,created=self.store.submit('reauth',account_id,'auto:'+recovery['generation']+':'+str(attempt),source='auto_reauth',extra={'target':target})
        if created:
            with self.store.transaction():self.store.db.execute('UPDATE recovery_state SET attempts=?,next_retry=?,job_id=? WHERE account_id=?',(attempt,self.store.clock()+900,recovery_job['id'],account_id))

    def periodic(self,job,check):
        target=self.target(job,True);auth=self.management.auth(target['auth_ref']);priority=int(auth.get('priority') or 0)
        plan=str(auth.get('plan_type') or '').lower()
        if auth.get('disabled') or not ('business' in plan or plan=='team'):raise OpsError('disabled' if auth.get('disabled') else 'free')
        first=self.management.candy(target['auth_ref'],check)
        correct=priority==0 and first is True
        if not correct and not (priority>0 and first is False):return
        if correct:
            for _ in range(240):check();time.sleep(1)
        repeated=[self.management.candy(target['auth_ref'],check),self.management.candy(target['auth_ref'],check)]
        desired=periodic_decision(priority,first,repeated)
        if desired is not None:
            check();self.revisions[job['id']]=self.management.patch(self.target(job,True),{'priority':desired})
            with self.store.transaction():self.store._event(job['id'],'quality','quality_passed' if desired==10 else 'quality_failed','quality','scheduled',job['account_id'])
            self.store.queue_notification(job['id'],'quality_passed' if desired==10 else 'quality_failed')

    def notify(self,job):
        self.store.queue_notification(job['id'])
        return False

    def flush_notifications(self):
        helper=self.config.get('notification_helper')
        if not helper:return
        for notification in self.store.pending_notifications():
            try:
                reply=self._helper(helper,{'notification_id':notification['id'],'job_id':notification['job_id'],'instance':self.store.instance,'reason':notification['reason']})
                sent=reply.get('sent') is True
            except (OpsError,subprocess.TimeoutExpired,OSError):sent=False
            self.store.notification_outcome(notification['id'],sent)

    def reconcile(self):
        from .processes import read_owner,is_alive
        with self.store.mutex:rows=self.store.db.execute("SELECT id FROM jobs WHERE state='waiting_human' AND reason='reconciliation_required' AND lease_until<=?",(self.store.clock(),)).fetchall()
        for row in rows:
            try:owner=read_owner(self.store.root/'jobs'/row['id']/'browser-owner.json')
            except OpsError:continue
            try:alive=is_alive(owner)
            except OpsError:continue
            if not alive:self.store.finish_reconciliation(row['id'])

    def cancel(self,job):
        if job['state']=='waiting_human' and job['reason']=='reconciliation_required':
            from .processes import read_owner,terminate
            terminate(read_owner(self.store.root/'jobs'/job['id']/'browser-owner.json'))
            self.store.finish_reconciliation(job['id']);return
        if job['state'] in ('succeeded','partial','failed','cancelled','skipped'):return
        private=self.store.root/'jobs'/job['id']
        private.mkdir(mode=0o700,parents=True,exist_ok=True)
        (private/'cancel').touch(mode=0o600)

    def resume(self,job):
        # The live child continuously inspects the same challenge page; this action
        # never creates an OAuth session or trusts the operator's click as proof.
        with self.process_mutex:
            process=self.processes.get(job['id'])
            if process is None or process.poll() is not None:raise OpsError('reconciliation_required',409)
        return self.store.get_job(job['id'])

    @staticmethod
    def _helper(path,payload):
        executable=Path(path)
        if not executable.is_absolute() or executable.is_symlink() or executable.stat().st_mode&0o022:raise OpsError('unavailable',503)
        try:reply=subprocess.run([str(executable)],input=json.dumps(payload).encode(),stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,check=False,timeout=120)
        except (OSError,subprocess.TimeoutExpired):raise OpsError('unavailable',503) from None
        if reply.returncode!=0 or len(reply.stdout)>16384:raise OpsError('unavailable',503)
        try:return json.loads(reply.stdout)
        except ValueError:raise OpsError('failed') from None
