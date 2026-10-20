"""Read historical files as data only; never import operational scripts."""
import datetime as dt
import hashlib
import json
from pathlib import Path
import re
from zoneinfo import ZoneInfo
from .security import OpsError, REASONS, email
from .store import encode

TZ=ZoneInfo('Asia/Shanghai')
SUMMARY=re.compile(r'^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) 糖果巡检 · gpt-6\.1-sol：(\d+) 个账号，跳过 (\d+) 个不符合套餐条件的账号$')


def timestamp(value):
    if isinstance(value,(float,int)) and not isinstance(value,bool): return value/1000 if value>10**12 else value
    try:
        parsed=dt.datetime.fromisoformat(str(value).replace('Z','+00:00'))
        if parsed.tzinfo is None: parsed=parsed.replace(tzinfo=TZ)
        return parsed.timestamp()
    except (ValueError,TypeError): return None


class Legacy:
    def __init__(self, store, paths=None): self.store,self.paths=store,paths or {}

    def refresh(self):
        summary=[]
        for kind,path in self.paths.items():
            if kind not in ('plugin','auto_reauth','health','inspection_log'): continue
            path=Path(path)
            if not path.exists():
                summary.append({'kind':kind,'available':False,'reason':'no_history'});continue
            try:
                size=path.stat().st_size
                if kind!='inspection_log' and size>32*1024*1024: raise OpsError('unavailable')
                if kind=='inspection_log':
                    with path.open('rb') as stream:
                        stream.seek(max(0,size-2*1024*1024)); text=stream.read().decode('utf-8',errors='replace')
                    info=self._log(text)
                else:
                    data=json.loads(path.read_text())
                    info=getattr(self,'_'+kind)(data)
                summary.append({'kind':kind,'available':True,'legacy':True,'time_precision':'observed' if kind in ('auto_reauth','health') else 'source',**info})
            except (OSError,ValueError,TypeError,KeyError,AttributeError,OverflowError,OpsError):
                summary.append({'kind':kind,'available':False,'reason':'unavailable'})
        return {'sources':summary,'history_complete':False,'observed_at':self.store.clock()}

    def _plugin(self,data):
        count=0; latest=None
        for reference,results in data.get('results',{}).items():
            for result in results:
                if not isinstance(result,dict):continue
                when=timestamp(result.get('time'))
                reason='quality_passed' if result.get('ok') is True and not result.get('error') and not result.get('skipped') else 'quality_failed'
                evidence='plugin:'+hashlib.sha256(encode([reference,result.get('time'),result.get('model'),result.get('effort'),reason]).encode()).hexdigest()
                self.store.legacy_event('quality',reason,evidence,when,phase='quality')
                count+=1
                if when is not None: latest=max(latest or when,when)
        return {'retained_results':count,'last_run':latest,'per_credential_limit':20}

    def _auto_reauth(self,data):
        counts={'accounts':0,'waiting_human':0,'attempts':0,'pending_notifications':0}
        for address,entry in data.get('accounts',{}).items():
            if not isinstance(entry,dict):continue
            try: account_id=self.store.account(email(address))
            except OpsError: continue
            reason=entry.get('last_reason','unknown')
            reason=reason if reason in REASONS else 'unknown'
            blocked=bool(entry.get('blocked'));attempts=entry.get('attempts',0)
            counts['accounts']+=1;counts['waiting_human']+=int(blocked)
            if isinstance(attempts,int) and not isinstance(attempts,bool):counts['attempts']+=max(0,min(attempts,2))
            when=timestamp(entry.get('finished_at'))
            evidence='auto:'+hashlib.sha256(encode([account_id,when,reason,blocked,attempts if isinstance(attempts,int) else 0]).encode()).hexdigest()
            self.store.legacy_event('auto_reauth_scan',reason,evidence,when,account_id)
        notifications=data.get('notifications',[])
        counts['pending_notifications']=len(notifications) if isinstance(notifications,(list,dict)) else 0
        return counts

    def _health(self,data):
        counts={'accounts':0,'unavailable':0,'unauthorized':0,'pending_notifications':0}
        for reference,entry in data.items():
            if not isinstance(entry,dict):continue
            counts['accounts']+=1
            for field,name in (('bad','unavailable'),('unauthorized','unauthorized'),('pending','pending_notifications')):
                counts[name]+=int(bool(entry.get(field)))
            reason='notification_pending' if entry.get('pending') else 'failed' if entry.get('bad') else 'healthy'
            # Historical health keys are opaque hashes. Do not guess an account association.
            evidence='health:'+hashlib.sha256(encode([reference,reason,entry.get('seen'),entry.get('success')]).encode()).hexdigest()
            self.store.legacy_event('health',reason,evidence,phase='notify' if entry.get('pending') else 'reconcile')
        return counts

    def _log(self,text):
        latest=None; accounts=None; skipped=None;count=0
        for line in text.splitlines():
            match=SUMMARY.fullmatch(line)
            if not match:continue
            when=timestamp(match[1]); count+=1
            self.store.legacy_event('quality_inspection','legacy','log:'+hashlib.sha256(line.encode()).hexdigest(),when,phase='quality')
            if latest is None or when>latest:latest=when;accounts=int(match[2]);skipped=int(match[3])
        return {'last_run':latest,'accounts':accounts,'skipped':skipped,'retained_runs':count,'bounded_tail':True}
