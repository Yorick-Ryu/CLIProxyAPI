"""One browser slot with leased execution and per-stage durable results."""
import contextlib
import hashlib
import os
import threading
import uuid
from .security import OpsError
from .store import TERMINAL,uid


class Engine:
    def __init__(self,store,adapter=None):
        self.store,self.adapter=store,adapter
        self.owner=uuid.uuid4().hex
        self.boot_id=open('/proc/sys/kernel/random/boot_id').read().strip() if os.path.exists('/proc/sys/kernel/random/boot_id') else 'local-'+self.owner
        self.process_start=str(os.getpid())+':'+self.owner
        self.stop=threading.Event()
        self.thread=None
        self.last_gc=0
        self.notification_thread=None
        self.scheduler=None

    def start(self):
        self.store.reconcile()
        if self.adapter and hasattr(self.adapter,'reconcile'):self.adapter.reconcile()
        if self.adapter and hasattr(self.adapter,'flush_notifications'):
            self.notification_thread=threading.Thread(target=self.notification_loop,daemon=True);self.notification_thread.start()
        self.thread=threading.Thread(target=self.loop,name='account-ops-worker',daemon=True);self.thread.start()

    def loop(self):
        while not self.stop.is_set():
            try:
                if self.store.clock()-self.last_gc>=60:
                    self.store.purge_expired_private_material();self.store.reconcile()
                    if self.adapter and hasattr(self.adapter,'reconcile'):self.adapter.reconcile()
                    self.last_gc=self.store.clock()
                if self.scheduler:self.scheduler.tick()
                self.tick()
            except Exception:
                # Persisted reconciliation is the recovery boundary; an exception string
                # can contain browser URLs and must not enter ordinary logging.
                self.store.reconcile()
            self.stop.wait(1)

    def notification_loop(self):
        while not self.stop.is_set():
            try:self.adapter.flush_notifications()
            except Exception:pass
            self.stop.wait(5)

    def close(self):
        self.stop.set()
        if self.thread:self.thread.join(timeout=5)
        if self.notification_thread:self.notification_thread.join(timeout=5)

    def tick(self):
        self.store.aggregate_batches()
        job=self.store.claim(self.owner,self.boot_id,self.process_start)
        if not job:return False
        keep_alive=threading.Event()
        def heartbeats():
            while not keep_alive.wait(15):
                try:
                    self.store.heartbeat(job['id'],self.owner)
                except OpsError:return
        heartbeat=threading.Thread(target=heartbeats,daemon=True);heartbeat.start()
        try:
            scope=self.adapter.exclusive(job) if self.adapter and hasattr(self.adapter,'exclusive') else contextlib.nullcontext()
            with scope:self.run(job)
        except OpsError as exc:
            phase=self.store.get_job(job['id'])['phase']
            state='cancelled' if exc.reason=='cancelled' else 'waiting_human' if exc.reason in ('human_required','workspace_mismatch','configuration_changed') else 'partial' if phase in ('proxy','quality','preferences','notify') else 'failed'
            try:self.store.transition(job['id'],self.owner,state,self.store.get_job(job['id'])['phase'],exc.reason)
            except OpsError:pass
            if self.adapter:self.store.queue_notification(job['id'],exc.reason)
        except Exception:
            try:self.store.transition(job['id'],self.owner,'failed',self.store.get_job(job['id'])['phase'],'failed')
            except OpsError:pass
        finally:
            keep_alive.set();heartbeat.join()
            self.store.aggregate_batches()
        return True

    def check(self,job):
        current=self.store.get_job(job['id'])
        if current['cancel_requested']:raise OpsError('cancelled',409)
        self.store.heartbeat(job['id'],self.owner)
        if self.adapter:self.adapter.guard(job)

    def phase(self,job,phase,reason='running'):
        self.check(job)
        self.store.transition(job['id'],self.owner,'running',phase,reason)

    def run(self,job):
        if job['kind']=='record':
            self.record_order(job)
            self.store.enroll_job_credentials(job['id'],self.owner)
            self.store.transition(job['id'],self.owner,'succeeded','persist_verify','recorded')
            return
        if self.adapter is None:raise OpsError('unavailable',503)
        if job['kind'] in ('quality_inspection','health_scan','auto_reauth_scan'):
            self.adapter.scheduled(job,lambda:self.check(job))
            self.store.transition(job['id'],self.owner,'succeeded','persist_verify','success');return
        if job['kind'] in ('login','reauth') and hasattr(self.adapter,'perform_login'):
            self.phase(job,'preflight')
            def report(phase,reason):
                state='waiting_human' if reason=='human_required' else 'running'
                self.store.transition(job['id'],self.owner,state,phase,reason)
                if reason=='human_required':self.store.queue_notification(job['id'],reason)
            self.adapter.perform_login(job,report,lambda:self.check(job))
        elif job['kind'] in ('login','reauth'):
            for phase in ('preflight','login_email','password','mfa','workspace','callback','persist_verify'):
                self.phase(job,phase);self.adapter.step(job,phase)
        if job['kind'] in ('login','reauth'):
            self.record_order(job)
            self.store.enroll_job_credentials(job['id'],self.owner)
            if hasattr(self.adapter,'enroll_automatic'):self.adapter.enroll_automatic(job)
        if job['kind']=='login':
            for phase in ('preferences','proxy'):
                self.phase(job,phase);self.adapter.step(job,phase)
        if job['kind'] in ('login','reauth','apply_preferences'):
            current,target=self.adapter.priorities(job)
            if target>current:
                self.phase(job,'quality')
                if job['snapshot'].get('quality_skip')=='user_explicit':
                    self.store.transition(job['id'],self.owner,'running','quality','user_skipped')
                elif not self.adapter.quality(job):
                    self.store.transition(job['id'],self.owner,'partial','quality','quality_failed')
                    return
                else:self.store.transition(job['id'],self.owner,'running','quality','quality_passed')
            self.phase(job,'preferences');self.adapter.step(job,'preferences');self.adapter.verify(job)
            self.phase(job,'notify')
            if not self.adapter.notify(job):
                self.store.queue_notification(job['id'])
                self.store.transition(job['id'],self.owner,'partial','notify','notification_pending');return
            self.store.transition(job['id'],self.owner,'succeeded','notify','success')

    def resume(self,job_id):
        job=self.store.get_job(job_id)
        if not self.adapter or job['state']!='waiting_human' or job['reason']!='human_required':raise OpsError('conflict',409)
        # The adapter must inspect the existing page. A button click is not evidence.
        self.adapter.resume(job)
        return self.store.get_job(job_id)

    def record_order(self,job):
        snapshot=job['snapshot']
        if 'draft_id' not in snapshot:return
        row=snapshot.get('order',{})
        with self.store.transaction():
            self.store.db.execute('INSERT OR IGNORE INTO orders VALUES(?,?,?,?,?,?)',(uid(),row.get('order_id'),job['account_id'],row.get('amount_cents'),snapshot['draft_id'],snapshot['row_index']))
