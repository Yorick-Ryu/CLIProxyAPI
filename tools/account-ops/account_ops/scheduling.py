"""A schedule changes owners only after the old entry is observably absent."""
import json
import os
import subprocess
from .security import OpsError,private_file

ENTRYPOINTS={'quality_inspection':('inspection.py',False),'health_scan':('inspection.py',True),'auto_reauth_scan':('cpa_auto_reauth.py',None)}


def legacy_present(kind,runner=subprocess.run):
    try:result=runner(['/usr/bin/crontab','-l'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,check=False)
    except OSError:raise OpsError('unavailable',503) from None
    if result.returncode not in (0,1):raise OpsError('unavailable',503)
    if result.returncode==1 and 'no crontab' not in result.stderr.lower():raise OpsError('unavailable',503)
    name,health=ENTRYPOINTS[kind]
    for line in result.stdout.splitlines():
        if line.lstrip().startswith('#') or name not in line:continue
        if health is None or ('--health-only' in line)==health:return True
    return False


class Scheduler:
    def __init__(self,store,executor):self.store,self.executor=store,executor
    def tick(self):
        if self.store.instance!='clouvider':return
        for row in self.store.schedules()['items']:
            if row['owner']!='service' or not row['enabled']:continue
            if row['next_run'] is not None and row['next_run']>self.store.clock():continue
            if legacy_present(row['kind']):
                self.store.legacy_event(row['kind'],'conflict','owner-conflict:'+row['kind']+':'+str(int(self.store.clock()//300)))
                continue
            interval=row['interval_seconds'];bucket=int(self.store.clock()//interval)
            job,_=self.store.submit(row['kind'],None,'schedule:'+row['kind']+':'+str(bucket),source='scheduled')
            with self.store.transaction():
                self.store.db.execute('UPDATE schedules SET last_run=?,next_run=? WHERE kind=? AND owner=\'service\'',(self.store.clock(),(bucket+1)*interval,row['kind']))
    def configure(self,kind,body):
        if self.store.instance!='clouvider' or kind not in ENTRYPOINTS:raise OpsError('invalid_input')
        if set(body)-{'version','enabled','owner','receipt_id'} or type(body.get('enabled')) is not bool or type(body.get('version')) is not int:raise OpsError('invalid_input')
        if body.get('owner')!='service':raise OpsError('invalid_input')
        if legacy_present(kind):raise OpsError('conflict',409)
        with self.store.mutex:prior=self.store.db.execute('SELECT * FROM schedules WHERE kind=?',(kind,)).fetchone()
        if prior['owner']!='service':
            receipt=json.loads(private_file(self.store.root/'ownership.json'))
            proof=receipt.get(kind,{})
            if proof.get('instance')!='clouvider' or proof.get('old_cron_removed') is not True or proof.get('id')!=body.get('receipt_id') or proof.get('expires_at',0)<=self.store.clock():raise OpsError('conflict',409)
        with self.store.transaction():
            changed=self.store.db.execute('UPDATE schedules SET version=version+1,enabled=?,owner=\'service\',next_run=? WHERE kind=? AND version=?',(int(body['enabled']),self.store.clock(),kind,body['version'])).rowcount
            if not changed:raise OpsError('conflict',409)
        return next(row for row in self.store.schedules()['items'] if row['kind']==kind)
