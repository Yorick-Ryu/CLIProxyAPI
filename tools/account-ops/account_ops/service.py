"""The public protocol always reports the executor's actual capabilities."""
import datetime as dt
import hashlib
import re
from .legacy import Legacy
from .parsing import parse,preview
from .security import OpsError, IDENTIFIER
from .store import DEFAULTS


class Service:
    def __init__(self,store,legacy=None,executor=None):
        self.store,self.legacy,self.executor=store,legacy or Legacy(store),executor
        self.scheduler=None

    def capabilities(self):
        can_execute=self.executor is not None
        return {'protocol_version':1,'instance':self.store.instance,'worker_available':True,
                'history':True,'imports':True,'record_only':True,'browser':can_execute,
                'proxy':bool(can_execute and self.executor.capabilities.get('proxy')),
                'human_takeover':bool(can_execute and self.executor.capabilities.get('human_takeover')),
                'schedule_owner':'legacy' if self.store.instance=='clouvider' else 'disabled','synthetic':bool(can_execute and self.executor.capabilities.get('synthetic'))}

    def dispatch(self,method,path,query,body,key=None):
        if match:=re.fullmatch(r'/_internal/jobs/([a-f0-9]{32})/guard',path):
            if method=='GET':return 200,self.store.commit_guard(match[1])
        if path=='/capabilities' and method=='GET':return 200,self.capabilities()
        if path=='/monitor' and method=='GET':return 200,{**self.legacy.refresh(),'schedules':self.store.schedules()['items'],'notifications':self.store.notification_summary()}
        if path=='/imports/preview' and method=='POST':
            if set(body)-{'text','instance'} or body.get('instance',self.store.instance)!=self.store.instance:raise OpsError('invalid_input')
            rows=parse(body.get('text'),dt.datetime.fromtimestamp(self.store.clock(),dt.timezone.utc))
            draft_id=self.store.draft(body['text'],rows)
            return 201,{'draft_id':draft_id,'instance':self.store.instance,'expires_at':self.store.clock()+3600,'rows':preview(rows)}
        if path=='/jobs' and method=='POST': return self.submit(body,key)
        if path=='/jobs' and method=='GET':
            return 200,self.store.jobs({k:query.get(k) for k in ('account_id','parent_id','kind','state','source')},self._limit(query),query.get('cursor'))
        if path=='/records' and method=='GET':
            return 200,self.store.records(self._cursor(query),self._limit(query),account_id=query.get('account_id'),kind=query.get('kind'),source=query.get('source'),since=self._time(query,'since'),until=self._time(query,'until'))
        if path=='/accounts' and method=='GET':
            if self.executor and hasattr(self.executor,'inventory'): self.executor.inventory()
            # No credential ciphertext, token payload or auth path is part of this projection.
            with self.store.mutex:
                rows=self.store.db.execute('SELECT a.id,a.instance,a.provider,a.email,a.workspace,a.version,c.version AS credential_version FROM accounts a LEFT JOIN credentials c ON a.id=c.account_id ORDER BY a.email,a.workspace LIMIT 200').fetchall()
            return 200,{'items':[dict(row)|{'has_credentials':row['credential_version'] is not None} for row in rows]}
        if match:=re.fullmatch(r'/jobs/([a-f0-9]{32})(?:/(events|cancel|resume))?',path):
            job_id,action=match.groups()
            if method=='GET' and action is None:return 200,self.store.get_job(job_id)
            if method=='GET' and action=='events':
                self.store.get_job(job_id)
                return 200,self.store.records(self._cursor(query),self._limit(query),job_id=job_id)
            if method=='POST' and action=='cancel':
                job=self.store.cancel(job_id)
                if self.executor and hasattr(self.executor,'cancel'): self.executor.cancel(job)
                return 200,self.store.get_job(job_id)
            if method=='POST' and action=='resume':
                if not self.executor:raise OpsError('unavailable',503)
                return 200,self.executor.resume(self.store.get_job(job_id))
        if match:=re.fullmatch(r'/preferences/([a-zA-Z0-9_.-]{1,64})',path):
            if method=='GET':return 200,self.store.preferences(match[1])
            if method=='PUT':
                if set(body)!={'version','value'} or type(body['version']) is not int:raise OpsError('invalid_input')
                return 200,self.store.put_preferences(match[1],body['version'],body['value'])
        if match:=re.fullmatch(r'/accounts/([a-f0-9]{32})/credentials',path):
            if method=='POST':
                if set(body)!={'password','totp'}:raise OpsError('invalid_input')
                return 200,self.store.set_credentials(match[1],body)
        if path=='/schedules' and method=='GET':return 200,self.store.schedules()
        if match:=re.fullmatch(r'/schedules/(quality_inspection|health_scan|auto_reauth_scan)',path):
            if method=='PUT' and self.scheduler:return 200,self.scheduler.configure(match[1],body)
        # Schedule takeover requires a separately verified owner handoff, never an API toggle.
        raise OpsError('invalid_input',404)

    def submit(self,body,key,parent_id=None,freeze=None):
        allowed={'kind','account_id','draft_id','row_index','instance','quality_skip','confirm_amount'}
        if set(body)-allowed or body.get('instance',self.store.instance)!=self.store.instance:raise OpsError('invalid_input')
        kind=body.get('kind')
        if body.get('draft_id') and 'row_index' not in body and kind in ('record','login'):return self.batch(body,key)
        if kind not in ('record','login','reauth','apply_preferences'):raise OpsError('invalid_input')
        if kind!='record' and self.executor is None:raise OpsError('unavailable',503)
        account_id=body.get('account_id'); extra={}; private_input=None
        if body.get('quality_skip'):
            if body['quality_skip']!='user_explicit':raise OpsError('invalid_input')
            extra['quality_skip']='user_explicit'
        if body.get('draft_id'):
            rows=self.store.read_draft(body['draft_id'])
            index=body.get('row_index')
            if type(index) is not int or not 0<=index<len(rows):raise OpsError('invalid_input')
            row=rows[index]
            if kind=='login' and (row['secrets'] is None or any(i!='confirm_per_account_amount' for i in row['issues'])):raise OpsError('invalid_input')
            if 'confirm_per_account_amount' in row['issues'] and body.get('confirm_amount') is not True:raise OpsError('invalid_input')
            if account_id:raise OpsError('invalid_input')
            if row['email']:account_id=self.store.account(row['email'])
            extra.update(draft_id=body['draft_id'],row_index=index,order={k:row[k] for k in ('order_id','amount_cents','expires_at')})
            private_input=row['secrets']
            # Credential enrollment occurs only after the job owns the identity; the draft
            # stays encrypted and is read by that one executor, not the HTTP response.
        elif kind=='record' or not account_id:raise OpsError('invalid_input')
        if kind!='record' and self.executor: extra.update(self.executor.snapshot(account_id,kind))
        if freeze:extra.update(preferences=freeze['preferences'],preference_version=freeze['preference_version'])
        job,created=self.store.submit(kind,account_id,key,parent_id=parent_id,extra=extra,private_input=private_input)
        return (202 if created else 200),job

    def batch(self,body,key):
        if body['kind']!='record' and self.executor is None:raise OpsError('unavailable',503)
        rows=self.store.read_draft(body['draft_id'])
        parent,created=self.store.submit('batch',None,key,extra={'draft_id':body['draft_id'],'child_count':len(rows),'batch_kind':body['kind']})
        if not created:return 200,parent
        for row in rows:
            child_key=hashlib.sha256((key+':'+str(row['index'])).encode()).hexdigest()
            try:
                _,child=self.submit(body|{'row_index':row['index']},child_key,parent['id'],parent['snapshot'])
            except OpsError:
                child,_=self.store.submit('record',None,child_key,parent_id=parent['id'],extra={'rejected':True})
                with self.store.transaction():
                    self.store.db.execute("UPDATE jobs SET state='failed',reason='invalid_input' WHERE id=?",(child['id'],))
                    self.store._event(child['id'],'parse','invalid_input')
            with self.store.transaction():self.store.db.execute('INSERT INTO batch_children VALUES(?,?,?)',(parent['id'],row['index'],child['id']))
        self.store.aggregate_batches()
        return 202,self.store.get_job(parent['id'])

    @staticmethod
    def _limit(query):
        try:value=int(query.get('limit',50))
        except (ValueError,TypeError):raise OpsError('invalid_input') from None
        if not 1<=value<=200:raise OpsError('invalid_input')
        return value

    @staticmethod
    def _cursor(query):
        try:value=int(query.get('after',0))
        except (ValueError,TypeError):raise OpsError('invalid_input') from None
        if value<0:raise OpsError('invalid_input')
        return value

    @staticmethod
    def _time(query,key):
        if key not in query:return None
        try:return dt.datetime.fromisoformat(query[key].replace('Z','+00:00')).timestamp()
        except (ValueError,TypeError):raise OpsError('invalid_input') from None
