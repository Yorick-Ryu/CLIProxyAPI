"""Transactional queue, immutable events and a separate encrypted credential boundary."""
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import threading
import time
import uuid
from .security import OpsError, REASONS, email, lock_email, totp

TERMINAL = frozenset(('succeeded', 'partial', 'failed', 'cancelled', 'skipped'))
PHASES = frozenset('parse preflight login_email password mfa workspace callback persist_verify proxy quality preferences notify reconcile'.split())
DEFAULTS = {'instance':'clouvider', 'websockets':True, 'priority':10, 'model':'gpt-6.1-sol',
            'effort':'low', 'proxy_policy':'independent_ipv6', 'auto_enroll':True}
KINDS = frozenset('login reauth apply_preferences quality_inspection health_scan auto_reauth_scan record batch'.split())


def encode(value): return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(',', ':'))

def uid(): return uuid.uuid4().hex


class Store:
    def __init__(self, root, vault, clock=time.time, instance="clouvider"):
        if instance not in ("clouvider","dmit","dmit-test"): raise OpsError("invalid_input")
        self.instance = instance
        self.root, self.vault, self.clock = Path(root), vault, clock
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.root.is_symlink() or self.root.stat().st_mode & 0o077:
            raise OpsError('unavailable', 503)
        self.mutex = threading.RLock()
        self.db = sqlite3.connect(self.root/'operations.sqlite3', isolation_level=None, check_same_thread=False)
        os.chmod(self.root/'operations.sqlite3', 0o600)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA foreign_keys=ON')
        self.db.executescript('''
        CREATE TABLE IF NOT EXISTS instances(id TEXT PRIMARY KEY);
        CREATE TABLE IF NOT EXISTS schema_version(version INTEGER PRIMARY KEY);
        INSERT OR IGNORE INTO schema_version VALUES(1);
        CREATE TABLE IF NOT EXISTS drafts(id TEXT PRIMARY KEY, created REAL NOT NULL, expires REAL NOT NULL, payload BLOB NOT NULL);
        CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY, instance TEXT NOT NULL, provider TEXT NOT NULL, email TEXT NOT NULL,
          identity TEXT NOT NULL, workspace TEXT NOT NULL DEFAULT '', auth_ref TEXT, version INTEGER NOT NULL DEFAULT 1,
          UNIQUE(instance,provider,identity,workspace));
        CREATE TABLE IF NOT EXISTS credentials(account_id TEXT PRIMARY KEY REFERENCES accounts(id), version INTEGER NOT NULL, payload BLOB NOT NULL);
        CREATE TABLE IF NOT EXISTS preferences(profile TEXT PRIMARY KEY, version INTEGER NOT NULL, value TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY, parent_id TEXT REFERENCES jobs(id), instance TEXT NOT NULL,
          account_id TEXT REFERENCES accounts(id), identity TEXT, kind TEXT NOT NULL, source TEXT NOT NULL,
          state TEXT NOT NULL, phase TEXT NOT NULL DEFAULT 'preflight', reason TEXT NOT NULL, created REAL NOT NULL, updated REAL NOT NULL,
          snapshot TEXT NOT NULL, credential_version INTEGER, idempotency_key TEXT UNIQUE, request_key TEXT,
          lease_owner TEXT, lease_until REAL, boot_id TEXT, process_start TEXT, cancel_requested INTEGER NOT NULL DEFAULT 0);
        CREATE UNIQUE INDEX IF NOT EXISTS active_identity ON jobs(instance,identity) WHERE identity IS NOT NULL AND state NOT IN ('succeeded','partial','failed','cancelled','skipped');
        CREATE TABLE IF NOT EXISTS batch_children(parent_id TEXT REFERENCES jobs(id),row_index INTEGER,job_id TEXT REFERENCES jobs(id),PRIMARY KEY(parent_id,row_index));
        CREATE TABLE IF NOT EXISTS job_inputs(job_id TEXT PRIMARY KEY REFERENCES jobs(id),payload BLOB NOT NULL);
        CREATE TABLE IF NOT EXISTS credential_enrollments(job_id TEXT PRIMARY KEY REFERENCES jobs(id),version INTEGER NOT NULL);
        CREATE TABLE IF NOT EXISTS events(cursor INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT REFERENCES jobs(id),
          instance TEXT NOT NULL, account_id TEXT, source TEXT NOT NULL, kind TEXT NOT NULL, phase TEXT NOT NULL,
          reason TEXT NOT NULL, time REAL NOT NULL, evidence_id TEXT UNIQUE);
        CREATE INDEX IF NOT EXISTS events_job ON events(job_id,cursor);
        CREATE TABLE IF NOT EXISTS recovery_state(account_id TEXT PRIMARY KEY REFERENCES accounts(id),generation TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_retry REAL NOT NULL DEFAULT 0,blocked INTEGER NOT NULL DEFAULT 0,job_id TEXT);
        CREATE TABLE IF NOT EXISTS health_state(account_id TEXT PRIMARY KEY REFERENCES accounts(id),bad INTEGER NOT NULL,event_id TEXT);
        CREATE TABLE IF NOT EXISTS schedules(kind TEXT PRIMARY KEY, version INTEGER NOT NULL, enabled INTEGER NOT NULL,
          owner TEXT NOT NULL CHECK(owner IN ('legacy','service')), interval_seconds INTEGER NOT NULL, last_run REAL, next_run REAL);
        CREATE TABLE IF NOT EXISTS notifications(id TEXT PRIMARY KEY, job_id TEXT, state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
          next_retry REAL NOT NULL, event_id INTEGER UNIQUE NOT NULL REFERENCES events(cursor), reason TEXT NOT NULL DEFAULT 'unknown');
        CREATE TABLE IF NOT EXISTS orders(id TEXT PRIMARY KEY, external_id TEXT, account_id TEXT, amount_cents INTEGER, draft_id TEXT NOT NULL,
          row_index INTEGER NOT NULL, UNIQUE(draft_id,row_index));
        CREATE TABLE IF NOT EXISTS cost_adjustments(id TEXT PRIMARY KEY, order_id TEXT NOT NULL REFERENCES orders(id),
          delta_cents INTEGER NOT NULL, reason_code TEXT NOT NULL, time REAL NOT NULL, idempotency_key TEXT UNIQUE NOT NULL);
        ''')
        with self.transaction():
            if 'reason' not in {row[1] for row in self.db.execute('PRAGMA table_info(notifications)')}:
                self.db.execute("ALTER TABLE notifications ADD COLUMN reason TEXT NOT NULL DEFAULT 'unknown'")
            self.db.execute('INSERT OR IGNORE INTO schema_version VALUES(2)')
            prior_instance=self.db.execute("SELECT id FROM instances").fetchone()
            if prior_instance and prior_instance["id"]!=self.instance: raise OpsError("conflict",409)
            self.db.execute("INSERT OR IGNORE INTO instances VALUES(?)",(self.instance,))
            defaults=DEFAULTS|{'instance':self.instance}
            if self.instance!='clouvider':defaults.update(auto_enroll=False,proxy_policy='preserve')
            self.db.execute('INSERT OR IGNORE INTO preferences VALUES(?,?,?)', ('default', 1, encode(defaults)))
            for kind, interval in (('quality_inspection',1800),('health_scan',300),('auto_reauth_scan',300)):
                self.db.execute('INSERT OR IGNORE INTO schedules(kind,version,enabled,owner,interval_seconds) VALUES(?,1,?,\'legacy\',?)',(kind,int(self.instance=='clouvider'),interval))

    @contextmanager
    def transaction(self):
        with self.mutex:
            self.db.execute('BEGIN IMMEDIATE')
            try:
                yield
                self.db.execute('COMMIT')
            except BaseException:
                self.db.execute('ROLLBACK')
                raise

    def close(self): self.db.close()

    def account(self, address, workspace='', auth_ref=None):
        address = email(address)
        with self.transaction():
            self.db.execute('INSERT OR IGNORE INTO accounts(id,instance,provider,email,identity,workspace,auth_ref) VALUES(?,(SELECT id FROM instances),\'codex\',?,?,?,?)',
                            (uid(),address,lock_email(address),workspace,auth_ref))
            row=self.db.execute('SELECT id FROM accounts WHERE instance=(SELECT id FROM instances) AND provider=\'codex\' AND identity=? AND workspace=?',(lock_email(address),workspace)).fetchone()
            return row['id']

    def set_credentials(self, account_id, secret):
        password = secret.get('password')
        if not isinstance(password,str) or not password or len(password)>4096: raise OpsError('invalid_input')
        payload={'password':password,'totp':totp(secret.get('totp',''))}
        with self.transaction():
            if not self.db.execute('SELECT 1 FROM accounts WHERE id=?',(account_id,)).fetchone(): raise OpsError('invalid_input',404)
            if self.db.execute('SELECT 1 FROM jobs WHERE account_id=? AND state NOT IN (\'succeeded\',\'partial\',\'failed\',\'cancelled\',\'skipped\')',(account_id,)).fetchone(): raise OpsError('locked',409)
            old=self.db.execute('SELECT version FROM credentials WHERE account_id=?',(account_id,)).fetchone()
            version=old['version']+1 if old else 1
            self.db.execute('INSERT OR REPLACE INTO credentials VALUES(?,?,?)',(account_id,version,self.vault.seal(payload,'credential:'+account_id+':'+str(version))))
            return {'has_credentials':True,'version':version}

    def credentials(self, account_id, version):
        with self.mutex:
            row=self.db.execute('SELECT * FROM credentials WHERE account_id=?',(account_id,)).fetchone()
        if not row or row['version']!=version: raise OpsError('configuration_changed',409)
        return self.vault.open(row['payload'],'credential:'+account_id+':'+str(version))

    def draft(self, text, rows):
        draft_id=uid(); now=self.clock()
        with self.transaction():
            self.db.execute('INSERT INTO drafts VALUES(?,?,?,?)',(draft_id,now,now+3600,self.vault.seal({'text':text,'rows':rows},'draft:'+draft_id)))
        return draft_id

    def read_draft(self, draft_id):
        with self.mutex: row=self.db.execute('SELECT * FROM drafts WHERE id=?',(draft_id,)).fetchone()
        if not row or row['expires']<=self.clock(): raise OpsError('draft_expired',410)
        return self.vault.open(row['payload'],'draft:'+draft_id)['rows']

    def preferences(self, profile='default'):
        with self.mutex: row=self.db.execute('SELECT * FROM preferences WHERE profile=?',(profile,)).fetchone()
        if not row: raise OpsError('invalid_input',404)
        return {'version':row['version'],'value':json.loads(row['value'])}

    def put_preferences(self, profile, version, value):
        if set(value)-set(DEFAULTS) or value.get('instance')!=self.instance or value.get('model')!='gpt-6.1-sol' or value.get('effort')!='low': raise OpsError('invalid_input')
        if self.instance!='clouvider' and value.get('auto_enroll'):raise OpsError('invalid_input')
        if type(value.get('priority')) is not int or not -100<=value['priority']<=100: raise OpsError('invalid_input')
        if any(type(value.get(k)) is not bool for k in ('websockets','auto_enroll')) or value.get('proxy_policy') not in ('independent_ipv6','preserve'): raise OpsError('invalid_input')
        with self.transaction():
            changed=self.db.execute('UPDATE preferences SET version=version+1,value=? WHERE profile=? AND version=?',(encode(value),profile,version)).rowcount
            if not changed: raise OpsError('conflict',409)
        return self.preferences(profile)

    def _event(self, job_id, phase, reason, kind='job', source='manual', account_id=None, evidence_id=None, when=None):
        if phase not in PHASES or reason not in REASONS or source not in ('manual','scheduled','auto_reauth','migration','legacy') or kind not in KINDS|{'job','quality','health','notification','legacy'}: raise OpsError('invalid_input')
        return self.db.execute('INSERT OR IGNORE INTO events(job_id,instance,account_id,source,kind,phase,reason,time,evidence_id) VALUES(?,(SELECT id FROM instances),?,?,?,?,?,?,?)',
            (job_id,account_id,source,kind,phase,reason,self.clock() if when is None else when,evidence_id)).lastrowid

    def submit(self, kind, account_id, key, source='manual', parent_id=None, extra=None, private_input=None):
        if kind not in KINDS or source not in ('manual','scheduled','auto_reauth','migration') or not isinstance(key,str) or not 8<=len(key)<=128: raise OpsError('invalid_input')
        request_key=encode([kind,account_id,source,parent_id,extra])
        with self.transaction():
            prior=self.db.execute('SELECT * FROM jobs WHERE idempotency_key=?',(key,)).fetchone()
            if prior:
                if prior['request_key']!=request_key: raise OpsError('conflict',409)
                return self._job(prior),False
            account=self.db.execute('SELECT * FROM accounts WHERE id=?',(account_id,)).fetchone() if account_id else None
            if account_id and not account: raise OpsError('invalid_input',404)
            if account and kind!='record':
                prior=self.db.execute('SELECT * FROM jobs WHERE instance=(SELECT id FROM instances) AND identity=? AND state NOT IN (\'succeeded\',\'partial\',\'failed\',\'cancelled\',\'skipped\')',(account['identity'],)).fetchone()
                if prior: return self._job(prior),False
            credential=self.db.execute('SELECT version FROM credentials WHERE account_id=?',(account_id,)).fetchone() if account_id else None
            pref=self.preferences()
            snapshot={'preferences':pref['value'],'preference_version':pref['version'],'account_version':account['version'] if account else None,**(extra or {})}
            job_id=uid(); now=self.clock()
            self.db.execute('INSERT INTO jobs(id,parent_id,instance,account_id,identity,kind,source,state,reason,created,updated,snapshot,credential_version,idempotency_key,request_key) VALUES(?,?,(SELECT id FROM instances),?,?,?,?,\'queued\',\'queued\',?,?,?,?,?,?)',
                (job_id,parent_id,account_id,account['identity'] if account and kind!='record' else None,kind,source,now,now,encode(snapshot),credential['version'] if credential else None,key,request_key))
            if private_input is not None:
                self.db.execute('INSERT INTO job_inputs VALUES(?,?)',(job_id,self.vault.seal(private_input,'job:'+job_id)))
            self._event(job_id,'preflight','queued',account_id=account_id,source=source)
            return self.get_job(job_id),True

    def _job(self, row):
        keys=('id','parent_id','instance','account_id','kind','source','state','phase','reason','created','updated','cancel_requested')
        value={k:row[k] for k in keys}; value['cancel_requested']=bool(value['cancel_requested'])
        value['snapshot']=json.loads(row['snapshot']); value['credential_version']=row['credential_version']
        value['allowed_actions']=['cancel'] if row['state'] not in TERMINAL else []
        if row['state']=='waiting_human' and row['reason']=='human_required': value['allowed_actions'].append('resume')
        return value

    def get_job(self, job_id):
        with self.mutex: row=self.db.execute('SELECT * FROM jobs WHERE id=?',(job_id,)).fetchone()
        if not row: raise OpsError('invalid_input',404)
        result=self._job(row)
        if row['kind']=='batch':
            with self.mutex:children=self.db.execute('SELECT row_index,job_id FROM batch_children WHERE parent_id=? ORDER BY row_index',(job_id,)).fetchall()
            result['children']=[{'row_index':child['row_index'],'job':self.get_job(child['job_id'])} for child in children]
        return result

    def jobs(self, filters=None, limit=50, before=None):
        filters=filters or {}; clauses=[]; args=[]
        for key in ('account_id','parent_id','kind','state','source'):
            if filters.get(key): clauses.append(key+'=?');args.append(filters[key])
        if before:
            # Stable pagination even when timestamps are identical.
            row=self.db.execute('SELECT created,id FROM jobs WHERE id=?',(before,)).fetchone()
            if not row: raise OpsError('invalid_input')
            clauses.append('(created,id)<(?,?)'); args.extend(row)
        query='SELECT * FROM jobs'+(' WHERE '+' AND '.join(clauses) if clauses else '')+' ORDER BY created DESC,id DESC LIMIT ?'
        with self.mutex: rows=self.db.execute(query,(*args,min(max(limit,1),200)+1)).fetchall()
        items=[self._job(row) for row in rows[:limit]]
        return {'items':items,'next_cursor':items[-1]['id'] if len(rows)>limit else None}

    def records(self, after=0, limit=50, job_id=None, account_id=None, kind=None, source=None, since=None, until=None):
        clauses=['cursor>?']; args=[after]
        for key,value in (('job_id',job_id),('account_id',account_id),('kind',kind),('source',source)):
            if value: clauses.append(key+'=?');args.append(value)
        if since is not None:clauses.append('time>=?');args.append(since)
        if until is not None:clauses.append('time<?');args.append(until)
        with self.mutex: rows=self.db.execute('SELECT * FROM events WHERE '+' AND '.join(clauses)+' ORDER BY cursor LIMIT ?',(*args,min(max(limit,1),200)+1)).fetchall()
        items=[dict(row) for row in rows[:limit]]
        return {'items':items,'next_cursor':items[-1]['cursor'] if items else after,'has_more':len(rows)>limit}

    def claim(self, owner, boot_id, process_start):
        with self.transaction():
            # Expiry never releases a browser slot: a missing heartbeat is not proof of exit.
            if self.db.execute('SELECT 1 FROM jobs WHERE state=\'running\' OR (state=\'waiting_human\' AND reason IN (\'human_required\',\'reconciliation_required\'))').fetchone(): return None
            row=self.db.execute('SELECT * FROM jobs WHERE state=\'queued\' AND kind!=\'batch\' ORDER BY created,id LIMIT 1').fetchone()
            if not row: return None
            self.db.execute('UPDATE jobs SET state=\'running\',reason=\'running\',lease_owner=?,lease_until=?,boot_id=?,process_start=?,updated=? WHERE id=?',
                (owner,self.clock()+60,boot_id,process_start,self.clock(),row['id']))
            self._event(row['id'],'preflight','running',source=row['source'],account_id=row['account_id'])
            return self.get_job(row['id'])

    def heartbeat(self, job_id, owner):
        with self.transaction():
            if not self.db.execute('UPDATE jobs SET lease_until=? WHERE id=? AND lease_owner=? AND state IN (\'running\',\'waiting_human\') AND lease_until>?',
                (self.clock()+60,job_id,owner,self.clock())).rowcount: raise OpsError('lease_lost',409)

    def transition(self, job_id, owner, state, phase, reason):
        if state not in TERMINAL|{'running','waiting_human','retry_wait'}: raise OpsError('invalid_input')
        with self.transaction():
            row=self.db.execute('SELECT * FROM jobs WHERE id=?',(job_id,)).fetchone()
            if not row or row['lease_owner']!=owner or row['state'] in TERMINAL or row['lease_until']<=self.clock(): raise OpsError('lease_lost',409)
            if row['cancel_requested'] and state!='cancelled': raise OpsError('cancelled',409)
            self._event(job_id,phase,reason,source=row['source'],account_id=row['account_id'])
            self.db.execute('UPDATE jobs SET state=?,phase=?,reason=?,updated=? WHERE id=?',(state,phase,reason,self.clock(),job_id))

    def cancel(self, job_id):
        with self.transaction():
            row=self.db.execute('SELECT * FROM jobs WHERE id=?',(job_id,)).fetchone()
            if not row: raise OpsError('invalid_input',404)
            if row['state'] in TERMINAL: return self._job(row)
            self.db.execute('UPDATE jobs SET cancel_requested=1,updated=? WHERE id=?',(self.clock(),job_id))
            if row['kind']=='batch':
                self.db.execute("UPDATE jobs SET cancel_requested=1,updated=? WHERE parent_id=? AND state NOT IN ('succeeded','partial','failed','cancelled','skipped')",(self.clock(),job_id))
                self.db.execute("UPDATE jobs SET state='cancelled',reason='cancelled' WHERE parent_id=? AND state='queued'",(job_id,))

            if row['state']=='queued' or (row['state']=='waiting_human' and row['reason'] in ('configuration_changed','workspace_mismatch')): self.db.execute('UPDATE jobs SET state=\'cancelled\',reason=\'cancelled\' WHERE id=?',(job_id,))
            self._event(job_id,row['phase'],'cancelled',source=row['source'],account_id=row['account_id'])
            return self.get_job(job_id)

    def reconcile(self):
        with self.transaction():
            rows=self.db.execute('SELECT * FROM jobs WHERE (state=\'running\' OR (state=\'waiting_human\' AND reason IN (\'human_required\',\'reconciliation_required\'))) AND lease_until<=?',(self.clock(),)).fetchall()
            for row in rows:
                self.db.execute('UPDATE jobs SET state=\'waiting_human\',phase=\'reconcile\',reason=\'reconciliation_required\',updated=? WHERE id=?',(self.clock(),row['id']))
                if row['reason']!='reconciliation_required': self._event(row['id'],'reconcile','reconciliation_required',source=row['source'],account_id=row['account_id'])
            return len(rows)

    def finish_reconciliation(self,job_id):
        with self.transaction():
            row=self.db.execute('SELECT * FROM jobs WHERE id=?',(job_id,)).fetchone()
            if not row or row['state']!='waiting_human' or row['reason']!='reconciliation_required' or row['lease_until']>self.clock():raise OpsError('conflict',409)
            state='cancelled' if row['cancel_requested'] else 'failed'
            reason='cancelled' if row['cancel_requested'] else 'reconciliation_required'
            self.db.execute('UPDATE jobs SET state=?,reason=?,updated=? WHERE id=?',(state,reason,self.clock(),job_id))
            self._event(job_id,'reconcile',reason,source=row['source'],account_id=row['account_id'])

    def legacy_event(self, kind, reason, evidence, when=None, account_id=None, phase='reconcile'):
        with self.transaction(): return self._event(None,phase,reason,kind,'legacy',account_id,evidence,when)

    def schedules(self):
        with self.mutex: rows=self.db.execute('SELECT * FROM schedules ORDER BY kind').fetchall()
        return {'items':[dict(r)|{'enabled':bool(r['enabled']),'running':None if self.instance=='clouvider' else False,'owner':r['owner'] if self.instance=='clouvider' else 'disabled'} for r in rows]}

    def backup(self, destination):
        destination=Path(destination)
        fd=os.open(destination,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600);os.close(fd)
        target=sqlite3.connect(destination)
        try:
            with self.mutex:self.db.backup(target)
        finally:target.close()

    def commit_guard(self,job_id):
        with self.mutex:
            row=self.db.execute('SELECT j.*,a.email FROM jobs j JOIN accounts a ON a.id=j.account_id WHERE j.id=?',(job_id,)).fetchone()
        if not row:return {'allowed':False}
        snapshot=json.loads(row['snapshot']);target=snapshot.get('target') or {}
        return {'allowed':row['state'] in ('running','waiting_human') and not row['cancel_requested'] and bool(row['lease_until'] and row['lease_until']>self.clock()),
                'email':row['email'],'target':target.get('auth_ref',''),'automatic':row['source']=='auto_reauth'}

    def purge_expired_private_material(self):
        with self.transaction():
            self.db.execute('DELETE FROM drafts WHERE expires<=?',(self.clock(),))
            cutoff=self.clock()-90*86400
            self.db.execute("DELETE FROM job_inputs WHERE job_id IN (SELECT id FROM jobs WHERE state IN ('succeeded','partial','failed','cancelled','skipped') AND updated<?)",(self.clock()-3600,))
            self.db.execute("DELETE FROM events WHERE time<? AND cursor NOT IN (SELECT event_id FROM notifications)",(cutoff,))
            self.db.execute("DELETE FROM credential_enrollments WHERE job_id IN (SELECT id FROM jobs WHERE state IN ('succeeded','partial','failed','cancelled','skipped') AND updated<?)",(cutoff,))
            self.db.execute("DELETE FROM batch_children WHERE parent_id IN (SELECT id FROM jobs WHERE updated<? AND state IN ('succeeded','partial','failed','cancelled','skipped')) AND job_id IN (SELECT id FROM jobs WHERE updated<? AND state IN ('succeeded','partial','failed','cancelled','skipped'))",(cutoff,cutoff))
            self.db.execute("DELETE FROM jobs WHERE state IN ('succeeded','partial','failed','cancelled','skipped') AND updated<? AND id NOT IN (SELECT job_id FROM events WHERE job_id IS NOT NULL) AND id NOT IN (SELECT job_id FROM job_inputs) AND id NOT IN (SELECT parent_id FROM jobs WHERE parent_id IS NOT NULL)",(cutoff,))

    def private_job_input(self,job_id):
        with self.mutex:row=self.db.execute('SELECT payload FROM job_inputs WHERE job_id=?',(job_id,)).fetchone()
        if not row:raise OpsError('missing_credentials')
        return self.vault.open(row['payload'],'job:'+job_id)

    def enroll_job_credentials(self,job_id,owner):
        with self.transaction():
            job=self.db.execute('SELECT * FROM jobs WHERE id=?',(job_id,)).fetchone()
            if not job or job['lease_owner']!=owner or job['lease_until']<=self.clock() or job['cancel_requested']:raise OpsError('lease_lost',409)
            if self.db.execute('SELECT 1 FROM credential_enrollments WHERE job_id=?',(job_id,)).fetchone():return
            row=self.db.execute('SELECT payload FROM job_inputs WHERE job_id=?',(job_id,)).fetchone()
            if not row:return
            secret=self.vault.open(row['payload'],'job:'+job_id)
            prior=self.db.execute('SELECT version FROM credentials WHERE account_id=?',(job['account_id'],)).fetchone()
            version=(prior['version'] if prior else 0)+1
            self.db.execute('INSERT OR REPLACE INTO credentials VALUES(?,?,?)',(job['account_id'],version,self.vault.seal({k:secret[k] for k in ('password','totp')},'credential:'+job['account_id']+':'+str(version))))
            self.db.execute('INSERT INTO credential_enrollments VALUES(?,?)',(job_id,version))

    def aggregate_batches(self):
        with self.transaction():
            parents=self.db.execute("SELECT * FROM jobs WHERE kind='batch' AND state NOT IN ('succeeded','partial','failed','cancelled','skipped')").fetchall()
            for parent in parents:
                rows=self.db.execute('SELECT j.state FROM jobs j JOIN batch_children b ON j.id=b.job_id WHERE b.parent_id=?',(parent['id'],)).fetchall()
                expected=json.loads(parent['snapshot']).get('child_count')
                if len(rows)!=expected or any(row['state'] not in TERMINAL for row in rows):continue
                state='succeeded' if all(row['state']=='succeeded' for row in rows) else 'failed' if all(row['state']=='failed' for row in rows) else 'partial'
                reason='success' if state=='succeeded' else 'failed'
                if parent['cancel_requested']:state,reason='cancelled','cancelled'
                self.db.execute('UPDATE jobs SET state=?,phase=\'persist_verify\',reason=?,updated=? WHERE id=?',(state,reason,self.clock(),parent['id']))
                self._event(parent['id'],'persist_verify',reason,source=parent['source'])

    def queue_notification(self,job_id,reason="success"):
        with self.transaction():
            prior=self.db.execute('SELECT id FROM notifications WHERE job_id=? AND reason=?',(job_id,reason)).fetchone()
            if prior:return prior['id']
            job=self.db.execute('SELECT * FROM jobs WHERE id=?',(job_id,)).fetchone()
            cursor=self._event(job_id,'notify','notification_pending','notification',job['source'],job['account_id'])
            notification_id=uid()
            self.db.execute('INSERT INTO notifications(id,job_id,state,next_retry,event_id,reason) VALUES(?,?,\'pending\',?,?,?)',(notification_id,job_id,self.clock(),cursor,reason))
            return notification_id

    def notification_summary(self):
        with self.mutex:
            count=self.db.execute("SELECT count(*),min(next_retry) FROM notifications WHERE state!='sent'").fetchone()
        return {'pending':count[0],'next_retry':count[1]}

    def pending_notifications(self):
        with self.mutex:rows=self.db.execute("SELECT id,job_id,event_id,attempts,reason FROM notifications WHERE state='pending' AND next_retry<=? ORDER BY next_retry LIMIT 20",(self.clock(),)).fetchall()
        return [dict(row) for row in rows]

    def notification_outcome(self,notification_id,sent):
        with self.transaction():
            row=self.db.execute('SELECT * FROM notifications WHERE id=?',(notification_id,)).fetchone()
            if not row or row['state']=='sent':return
            attempt=row['attempts']+1
            self.db.execute('UPDATE notifications SET state=?,attempts=?,next_retry=? WHERE id=?',('sent' if sent else 'pending',attempt,self.clock()+min(900*2**min(attempt-1,7),86400),notification_id))
            job=self.db.execute('SELECT * FROM jobs WHERE id=?',(row['job_id'],)).fetchone()
            if sent:
                self._event(job['id'],'notify','success','notification',job['source'],job['account_id'])
                if row['reason']=='success' and job['state']=='partial' and job['phase']=='notify' and job['reason']=='notification_pending':
                    self.db.execute("UPDATE jobs SET state='succeeded',reason='success',updated=? WHERE id=?",(self.clock(),job['id']))
