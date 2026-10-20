import concurrent.futures
import datetime as dt
import json
import os
from pathlib import Path
import tempfile
import unittest
from account_ops.security import OpsError,Vault
from account_ops.parsing import parse,preview
from account_ops.store import Store,DEFAULTS
from account_ops.service import Service
from account_ops.engine import Engine
from account_ops.legacy import Legacy

SECRET='Synthetic-$&!--password----tail'
TOTP='JBSWY3DPEHPK3PXP'
TRIPLE=f'Sample+alias@EXAMPLE.com----{SECRET}----{TOTP}'


class Clock:
    def __init__(self):self.now=dt.datetime(2026,10,20,10,tzinfo=dt.timezone.utc).timestamp()
    def __call__(self):return self.now


class Fixture(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.root=Path(self.temp.name);self.clock=Clock()
        key=self.root/'master';key.write_bytes(os.urandom(32));key.chmod(0o600)
        self.vault=Vault(key);self.store=Store(self.root,self.vault,self.clock);self.service=Service(self.store)
    def tearDown(self):self.store.close();self.temp.cleanup()
    def draft(self,text=TRIPLE):
        return self.service.dispatch('POST','/imports/preview',{}, {'text':text})[1]
    def record(self,key='idempotent-key'):
        draft=self.draft();return self.service.submit({'kind':'record','draft_id':draft['draft_id'],'row_index':0},key)[1]


class ParsingTests(Fixture):
    def test_special_password_and_alias(self):
        row=parse(TRIPLE,dt.datetime.now(dt.timezone.utc))[0]
        self.assertEqual(row['email'],'Sample+alias@example.com');self.assertEqual(row['secrets']['password'],SECRET)
        safe=json.dumps(preview([row]));self.assertNotIn(SECRET,safe);self.assertNotIn(TOTP,safe)
    def test_legacy_separator_preserves_password_hyphens(self):
        row=parse(f'a@example.com--pass--with-hyphens--{TOTP}',dt.datetime.now(dt.timezone.utc))[0]
        self.assertEqual(row['secrets']['password'],'pass--with-hyphens')
    def test_no_raw_error_echo(self):
        with self.assertRaises(OpsError) as caught:self.draft(f'a@example.com----{SECRET}----invalid!!!')
        self.assertEqual(str(caught.exception),'invalid_input')
    def test_only_cdk_records_without_browser(self):
        draft=self.draft('订单号：ABC-123\nCDK: synthetic-redemption-code\n¥77')
        self.assertEqual(draft['rows'][0]['issues'],['missing_credentials'])
        self.assertNotIn('synthetic-redemption-code',json.dumps(draft))
        job=self.service.submit({'kind':'record','draft_id':draft['draft_id'],'row_index':0},'cdk-record-key')[1]
        Engine(self.store).tick();self.assertEqual(self.store.get_job(job['id'])['reason'],'recorded')
    def test_fixed_amount_and_relative_date(self):
        draft=self.draft('订单号：ABC-123\n¥77.10\n今晚18点到期\n'+TRIPLE)
        row=draft['rows'][0];self.assertEqual(row['amount_cents'],7710)
        self.assertEqual(row['expires_at'],'2026-10-20T18:00:00+08:00')
    def test_conflicting_dates_are_explicit(self):
        row=self.draft('明天9点到期\n有效期：2026-10-22 09:00\n'+TRIPLE)['rows'][0]
        self.assertIn('expiry_conflict',row['issues'])
    def test_multi_account_order_does_not_duplicate_cost_silently(self):
        draft=self.draft('订单号：ABC-123\n¥77\n'+TRIPLE+'\na2@example.com----password----'+TOTP)
        self.assertTrue(all('confirm_per_account_amount' in row['issues'] for row in draft['rows']))
        with self.assertRaises(OpsError):self.service.submit({'kind':'record','draft_id':draft['draft_id'],'row_index':0},'ambiguous-cost')
    def test_separate_orders_keep_amounts(self):
        rows=self.draft('订单号：ABC-123\n¥77\n'+TRIPLE+'\n订单号：XYZ-456\n78元\na2@example.com----password----'+TOTP)['rows']
        self.assertEqual([r['amount_cents'] for r in rows],[7700,7800])
    def test_draft_secret_at_rest_and_expiry(self):
        draft=self.draft();blob=self.store.db.execute('SELECT payload FROM drafts').fetchone()[0]
        self.assertNotIn(SECRET.encode(),blob);self.assertNotIn(TOTP.encode(),blob)
        self.clock.now+=3601
        with self.assertRaises(OpsError) as caught:self.store.read_draft(draft['draft_id'])
        self.assertEqual(caught.exception.status,410)
    def test_vault_purpose_binding(self):
        blob=self.vault.seal({'password':SECRET},'first')
        with self.assertRaises(Exception):self.vault.open(blob,'other')
    def test_non_private_master_key_refused(self):
        key=self.root/'public-key';key.write_bytes(os.urandom(32));key.chmod(0o644)
        with self.assertRaises(OpsError):Vault(key)


class QueueTests(Fixture):
    def test_duplicate_submission_returns_same_job(self):
        draft=self.draft();body={'kind':'record','draft_id':draft['draft_id'],'row_index':0}
        first=self.service.submit(body,'same-key-123')[1];second=self.service.submit(body,'same-key-123')[1]
        self.assertEqual(first['id'],second['id'])
    def test_idempotency_key_cannot_target_another_request(self):
        self.record('same-key-123')
        with self.assertRaises(OpsError) as caught:self.record('same-key-123')
        self.assertEqual(caught.exception.status,409)
    def test_manual_and_automatic_share_email_lock(self):
        a=self.store.account('Sample+alias@example.com','team-one')
        b=self.store.account('sample+alias@example.com','team-two')
        first=self.store.submit('reauth',a,'manual-1234')[0]
        second=self.store.submit('reauth',b,'auto-12345',source='auto_reauth')[0]
        self.assertEqual(first['id'],second['id'])
    def test_concurrent_submission_starts_only_one(self):
        account=self.store.account('a@example.com')
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
            ids=list(executor.map(lambda i:self.store.submit('reauth',account,f'concurrent-{i}')[0]['id'],range(8)))
        self.assertEqual(len(set(ids)),1)
    def test_global_browser_slot_and_dead_lease_require_reconciliation(self):
        a=self.record();self.store.claim('owner','boot-A','pid-start-A');b=self.store.submit('health_scan',None,'other-job-key')[0]
        self.assertIsNone(self.store.claim('second','boot-B','pid-start-B'))
        self.clock.now+=61;self.store.reconcile()
        self.assertEqual(self.store.get_job(a['id'])['reason'],'reconciliation_required')
        self.assertIsNone(self.store.claim('second','boot-B','pid-start-B'))
    def test_cancel_queued_is_idempotent(self):
        job=self.record();self.store.cancel(job['id']);self.store.cancel(job['id'])
        self.assertEqual(self.store.get_job(job['id'])['state'],'cancelled')
        self.assertIsNone(self.store.claim('owner','boot','start'))
    def test_cancel_running_does_not_free_slot_before_ack(self):
        job=self.record();self.store.claim('owner','boot','start');self.store.cancel(job['id'])
        self.assertIsNone(self.store.claim('owner2','boot','start2'))
        self.store.transition(job['id'],'owner','cancelled','preflight','cancelled')
    def test_frozen_preferences_and_conflict(self):
        job=self.record();self.store.put_preferences('default',1,DEFAULTS|{'priority':20})
        self.assertEqual(self.store.get_job(job['id'])['snapshot']['preferences']['priority'],10)
        with self.assertRaises(OpsError):self.store.put_preferences('default',1,DEFAULTS)
    def test_default_never_includes_cooling(self):self.assertNotIn('disable_cooling',self.store.preferences()['value'])
    def test_credential_version_and_no_echo(self):
        account=self.store.account('a@example.com')
        result=self.store.set_credentials(account,{'password':SECRET,'totp':TOTP})
        self.assertEqual(result,{'has_credentials':True,'version':1})
        blob=self.store.db.execute('SELECT payload FROM credentials').fetchone()[0]
        self.assertNotIn(SECRET.encode(),blob)
        self.assertEqual(self.store.credentials(account,1)['password'],SECRET)
        self.store.submit('reauth',account,'credentials-lock')
        with self.assertRaises(OpsError):self.store.set_credentials(account,{'password':SECRET,'totp':TOTP})
    def test_event_free_text_is_rejected(self):
        with self.assertRaises(OpsError):self.store.legacy_event('health',SECRET,'safe-evidence')
    def test_record_retries_do_not_duplicate_cost(self):
        draft=self.draft('订单号：ABC-123\n¥77\n'+TRIPLE);body={'kind':'record','draft_id':draft['draft_id'],'row_index':0}
        job=self.service.submit(body,'record-once-1')[1];engine=Engine(self.store);engine.tick()
        job2=self.service.submit(body,'record-twice-2')[1];engine.tick()
        self.assertEqual(self.store.db.execute('SELECT count(*) FROM orders').fetchone()[0],1)
    def test_missing_browser_returns_unavailable_before_enqueue(self):
        draft=self.draft()
        with self.assertRaises(OpsError) as caught:self.service.submit({'kind':'login','draft_id':draft['draft_id'],'row_index':0},'unavailable-key')
        self.assertEqual(caught.exception.status,503);self.assertEqual(len(self.store.jobs()['items']),0)
    def test_jobs_pagination_same_timestamp(self):
        for i in range(5):self.store.submit('health_scan',None,f'pagination-{i}')
        first=self.store.jobs(limit=2);second=self.store.jobs(limit=2,before=first['next_cursor'])
        self.assertFalse(set(j['id'] for j in first['items']) & set(j['id'] for j in second['items']))
    def test_backup_is_consistent_and_private(self):
        self.record();backup=self.root/'backup.sqlite3';self.store.backup(backup)
        self.assertEqual(backup.stat().st_mode & 0o777,0o600)
    def test_cross_instance_and_path_injection_refused(self):
        for method,path,body in [('POST','/imports/preview',{'instance':'dmit','text':TRIPLE}),('GET','/preferences/../../master',{}),('GET','/accounts/../../master/credentials',{})]:
            with self.assertRaises(OpsError):self.service.dispatch(method,path,{},body)


class LegacyTests(Fixture):
    def test_plugin_answer_and_errors_are_not_exported(self):
        path=self.root/'plugin.json';path.write_text(json.dumps({'results':{'auth-name':[{'time':'2026-10-20T10:00:00Z','model':'gpt-6.1-sol','effort':'low','ok':False,'answer':SECRET,'error':TOTP}]}}))
        legacy=Legacy(self.store,{'plugin':path});legacy.refresh();legacy.refresh()
        records=self.store.records()['items'];self.assertEqual(len(records),1)
        self.assertNotIn(SECRET,json.dumps(records));self.assertNotIn(TOTP,json.dumps(records))
        self.assertEqual(records[0]['reason'],'quality_failed')
    def test_health_and_auto_state_only_allow_enumerated_fields(self):
        path=self.root/'auto.json';path.write_text(json.dumps({'accounts':{'a@example.com':{'last_reason':SECRET,'job':'/private/'+TOTP,'pid':123,'blocked':True,'attempts':2}},'notifications':[SECRET]}))
        summary=Legacy(self.store,{'auto_reauth':path}).refresh()
        self.assertEqual(summary['sources'][0]['pending_notifications'],1)
        public=json.dumps(summary)+json.dumps(self.store.records())
        self.assertNotIn(SECRET,public);self.assertNotIn(TOTP,public)
    def test_log_is_parsed_by_strict_pattern(self):
        path=self.root/'inspection.log';path.write_text(SECRET+'\n2026-10-20 18:00:00 糖果巡检 · gpt-6.1-sol：5 个账号，跳过 2 个不符合套餐条件的账号\n'+TOTP)
        result=Legacy(self.store,{'inspection_log':path}).refresh()
        self.assertEqual(result['sources'][0]['accounts'],5)
        self.assertNotIn(SECRET,json.dumps(self.store.records()))
    def test_missing_history_is_not_reported_as_complete(self):
        result=Legacy(self.store,{'plugin':self.root/'absent'}).refresh()
        self.assertFalse(result['history_complete']);self.assertEqual(result['sources'][0]['reason'],'no_history')




class PolicyTests(unittest.TestCase):
    def test_auto_eligibility_does_not_treat_network_as_unauthorized(self):
        from account_ops.policy import automatic
        now=dt.datetime(2026,10,20,10,tzinfo=dt.timezone.utc).timestamp()
        base={'note':'到期时间：2026-10-21 18:00（北京时间）','plan_type':'team','status':'error','status_message':'401 unauthorized'}
        automatic(base,now)
        for change in ({'status_message':'429 rate limit'},{'status_message':'network timeout'},{'disabled':True},{'plan_type':'free'},{'note':''},{'note':'到期时间：2026-10-20 18:00（北京时间）'}):
            with self.assertRaises(OpsError):automatic(base|change,now)
    def test_periodic_requires_three_confirmations(self):
        from account_ops.policy import periodic_decision
        self.assertEqual(periodic_decision(10,False,[False,False]),0)
        self.assertEqual(periodic_decision(0,True,[True,True]),10)
        for args in ((0,True,[False,True]),(10,False,[False,True]),(0,None,[True,True]),(10,False,None)):
            self.assertIsNone(periodic_decision(*args))
    def test_ipv6_history_and_cooldown_order(self):
        from account_ops.policy import ipv6_candidates
        now=300000
        rows=[{'address':'unknown'},{'address':'23h','released_at':now-23*3600},{'address':'24h','released_at':now-24*3600},{'address':'48h','released_at':now-48*3600},{'address':'new','never_used':True,'created_at':now},{'address':'disabled-bound','never_used':True,'created_at':now,'bound_to':'disabled-account'}]
        self.assertEqual([row['address'] for row in ipv6_candidates(rows,{'occupied'},now)],['new','48h','24h'])

class BatchTests(Fixture):
    def test_batch_has_parent_children_and_independent_costs(self):
        draft=self.draft('订单号：BATCH-1\n77元\n'+TRIPLE+'\n订单号：BATCH-2\n78元\na2@example.com----password----'+TOTP)
        status,parent=self.service.submit({'kind':'record','draft_id':draft['draft_id']},'batch-parent-key')
        self.assertEqual(status,202);self.assertEqual(len(parent['children']),2)
        engine=Engine(self.store);engine.tick();engine.tick()
        self.assertEqual(self.store.get_job(parent['id'])['state'],'succeeded')
        self.assertEqual(self.store.db.execute('SELECT sum(amount_cents) FROM orders').fetchone()[0],15500)
        self.assertEqual(len(self.store.jobs({'parent_id':parent['id']})['items']),2)
    def test_record_freezes_input_beyond_draft_lifetime(self):
        job=self.record();self.clock.now+=3601;self.store.purge_expired_private_material()
        Engine(self.store).tick();self.assertEqual(self.store.get_job(job['id'])['state'],'succeeded')
        with self.store.mutex:row=self.store.db.execute('SELECT account_id,version FROM credentials').fetchone()
        self.assertEqual(self.store.credentials(row['account_id'],row['version'])['password'],SECRET)
    def test_commit_guard_denies_cancel_and_dead_lease(self):
        job=self.record();self.store.claim('worker','boot','start')
        self.assertTrue(self.store.commit_guard(job['id'])['allowed'])
        self.clock.now+=61;self.assertFalse(self.store.commit_guard(job['id'])['allowed'])
        self.store.cancel(job['id']);self.assertFalse(self.store.commit_guard(job['id'])['allowed'])

class NotificationTests(Fixture):
    def test_outbox_retry_is_independent_and_idempotent(self):
        job=self.record();engine=Engine(self.store);engine.tick()
        first=self.store.queue_notification(job['id'],'success')
        self.assertEqual(first,self.store.queue_notification(job['id'],'success'))
        self.assertNotEqual(first,self.store.queue_notification(job['id'],'human_required'))
        self.store.notification_outcome(first,False)
        self.assertEqual(self.store.db.execute('SELECT attempts FROM notifications WHERE id=?',(first,)).fetchone()[0],1)
        self.assertNotIn(first,[row['id'] for row in self.store.pending_notifications()])
        self.clock.now+=900;self.assertIn(first,[row['id'] for row in self.store.pending_notifications()])
        self.store.notification_outcome(first,True);self.store.notification_outcome(first,True)
        self.assertEqual(self.store.notification_summary()['pending'],1)
        self.assertEqual(self.store.get_job(job['id'])['state'],'succeeded')
    def test_configuration_review_does_not_block_other_accounts(self):
        job=self.record();self.store.claim('worker','boot','start')
        self.store.transition(job['id'],'worker','waiting_human','callback','configuration_changed')
        other=self.store.submit('health_scan',None,'independent-other')[0]
        claimed=self.store.claim('second','boot','start2');self.assertEqual(claimed['id'],other['id'])
        self.store.cancel(job['id']);self.assertEqual(self.store.get_job(job['id'])['state'],'cancelled')

class ExecutionPolicyTests(Fixture):
    def adapter(self):
        from account_ops.execution import Executor
        adapter=object.__new__(Executor);adapter.store=self.store;adapter.revisions={};adapter.config={}
        self.account=self.store.account('a@example.com','workspace','codex-a.json')
        self.store.set_credentials(self.account,{'password':SECRET,'totp':TOTP})
        self.target={'id':'a'*32,'email':'a@example.com','workspace':'workspace','auth_ref':'codex-a.json','config_version':'one','plan_type':'team'}
        class Management:
            def auth(_,name):return {'note':'到期时间：2026-10-22 18:00（北京时间）','plan_type':'team'}
        adapter.management=Management();return adapter
    def test_retry_wait_starts_after_failure(self):
        adapter=self.adapter();scan={'id':'scan'};bad={'status':'error','status_message':'401 unauthorized'}
        adapter.auto_candidate(scan,self.account,self.target,bad)
        row=self.store.db.execute('SELECT * FROM recovery_state').fetchone();first=self.store.get_job(row['job_id'])
        self.store.claim('owner','boot','start')
        self.clock.now+=1000
        self.store.db.execute("UPDATE jobs SET state='failed',updated=? WHERE id=?",(self.clock(),first['id']))
        adapter.auto_candidate(scan,self.account,self.target,bad)
        self.assertEqual(self.store.db.execute('SELECT attempts FROM recovery_state').fetchone()[0],1)
        self.clock.now+=900;adapter.auto_candidate(scan,self.account,self.target,bad)
        self.assertEqual(self.store.db.execute('SELECT attempts FROM recovery_state').fetchone()[0],2)
    def test_healthy_scan_never_reauthorizes(self):
        adapter=self.adapter();adapter.auto_candidate({'id':'scan'},self.account,self.target,{'status':'active','unavailable':False})
        self.assertEqual(self.store.jobs()['items'],[])
    def test_cancelled_recovery_requires_manual_action(self):
        adapter=self.adapter();bad={'status':'error','status_message':'401 unauthorized'}
        adapter.auto_candidate({'id':'scan'},self.account,self.target,bad)
        row=self.store.db.execute('SELECT * FROM recovery_state').fetchone();self.store.cancel(row['job_id'])
        self.clock.now+=1000;adapter.auto_candidate({'id':'scan'},self.account,self.target,bad)
        self.assertTrue(self.store.db.execute('SELECT blocked FROM recovery_state').fetchone()[0])
    def test_dmit_defaults_and_server_guard_disable_automatic(self):
        self.store.close();self.store=Store(self.root/'dmit',self.vault,self.clock,instance='dmit')
        pref=self.store.preferences();self.assertFalse(pref['value']['auto_enroll']);self.assertEqual(pref['value']['proxy_policy'],'preserve')
        self.assertTrue(all(not row['enabled'] for row in self.store.schedules()['items']))
        with self.assertRaises(OpsError):self.store.put_preferences('default',pref['version'],pref['value']|{'auto_enroll':True})
        from account_ops.scheduling import Scheduler
        Scheduler(self.store,None).tick();self.assertEqual(self.store.jobs()['items'],[])
    def test_failed_quality_never_applies_priority(self):
        class Adapter:
            def __init__(_):_.phases=[]
            def guard(_,job):pass
            def step(_,job,phase):_.phases.append(phase)
            def priorities(_,job):return 0,10
            def quality(_,job):return False
        adapter=Adapter();account=self.store.account('a@example.com')
        job,_=self.store.submit('login',account,'failed-quality')
        Engine(self.store,adapter).tick()
        self.assertEqual(self.store.get_job(job['id'])['state'],'partial')
        self.assertEqual(adapter.phases.count('preferences'),1)
        self.assertNotIn('notify',adapter.phases)
        self.assertEqual(self.store.notification_summary()['pending'],1)
    def test_new_login_expiry_is_not_written_on_reauthorization(self):
        adapter=self.adapter();writes=[];auth={'access_token':'synthetic','account_id':'workspace','websockets':False,'priority':0}
        adapter.target=lambda job,allow_new=False:self.target
        adapter.management.auth=lambda name:auth
        adapter.management.patch=lambda target,fields:writes.append(fields) or 'two'
        job,_=self.store.submit('reauth',self.account,'expiry-reauth',extra={'order':{'expires_at':'2026-10-22T18:00:00+08:00'}})
        adapter.step(job,'preferences');self.assertEqual(writes,[])
        self.store.cancel(job['id'])
        job,_=self.store.submit('login',self.account,'expiry-newlogin',extra={'order':{'expires_at':'2026-10-22T18:00:00+08:00'}})
        adapter.step(job,'preferences');self.assertEqual(writes[0],{'websockets':True,'note':'到期时间：2026-10-22 18:00（北京时间）'})

class PrivateRetentionTests(Fixture):
    def test_terminal_debug_material_expires_without_touching_waiting_jobs(self):
        done=self.record();Engine(self.store).tick()
        waiting,_=self.store.submit('health_scan',None,'waiting-retention')
        for job in (done,waiting):
            directory=self.root/'jobs'/job['id'];directory.mkdir(parents=True);(directory/'browser.log').write_text('synthetic-private-debug')
        self.clock.now+=3601;self.store.purge_expired_private_material()
        self.assertFalse((self.root/'jobs'/done['id']).exists())
        self.assertTrue((self.root/'jobs'/waiting['id']/'browser.log').exists())

class LiveHistoryTests(Fixture):
    def test_plugin_api_history_is_linked_redacted_and_deduplicated(self):
        from account_ops.execution import Executor
        executor=object.__new__(Executor);executor.store=self.store
        account=self.store.account('a@example.com','team','codex-a.json')
        class Management:
            def api(_,method,path):return {'auths':[{'id':'codex-a.json','results':[{'time':'2026-10-20T10:00:00Z','model':'gpt-6.1-sol','ok':False,'error':'401 unauthorized '+SECRET}]}]}
        executor.management=Management();executor.legacy_plugin();executor.legacy_plugin()
        records=self.store.records()['items'];self.assertEqual(len(records),1)
        self.assertEqual(records[0]['kind'],'health');self.assertEqual(records[0]['account_id'],account)
        self.assertNotIn(SECRET,json.dumps(records))

class CandyEvidenceTests(unittest.TestCase):
    def test_fresh_result_is_captured_without_exposing_its_body(self):
        from account_ops.cpa import Management
        for answer,error,skipped,expected in [('21',None,False,True),('29',None,False,False),('',None,False,False),('', '401 unauthorized',False,None),('',None,True,None)]:
            with self.subTest(expected=expected,error=error,skipped=skipped):
                manager=object.__new__(Management);state=[0];evidence=[]
                def api(method,path,body=None):
                    if path.endswith('/state'):
                        state[0]+=1
                        return {'auths':[{'id':'codex-a.json','running':False,'results':[] if state[0]==1 else [{'time':'2026-10-20T10:00:00Z','model':'gpt-6.1-sol','effort':'low','ok':answer=='21','answer':answer,'error':error,'skipped':skipped}]}]}
                    if '/models?' in path:return {'models':[{'id':'gpt-6.1-sol'}]}
                    return {}
                manager.api=api
                self.assertIs(manager.candy('codex-a.json',lambda:None,wait=lambda _:None,record=lambda key,result:evidence.append(key)),expected)
                self.assertEqual(len(evidence),1);self.assertRegex(evidence[0],r'^candy:[a-f0-9]{64}$')

if __name__=='__main__':unittest.main()
