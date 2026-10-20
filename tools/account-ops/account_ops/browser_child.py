"""A single-account compatibility boundary; no global replacement is shared across jobs."""
import contextlib
import json
import logging
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.parse
from .cpa import Management
from .policy import automatic
from .security import OpsError


def main():
    os.umask(0o077)
    request=json.load(sys.stdin)
    private=Path(request['private_dir']);private.mkdir(mode=0o700,parents=True,exist_ok=True)
    output=sys.stdout
    def emit(phase,reason='running'):
        output.write(json.dumps({'phase':phase,'reason':reason})+'\n');output.flush()
    display=None
    def guard():
        job=management.api('GET','/v8/management/account-ops/jobs/'+request['job_id'])
        if job.get('cancel_requested'):raise OpsError('cancelled',409)
        if job.get('state') not in ('running','waiting_human') or job.get('reason')=='reconciliation_required':raise OpsError('lease_lost',409)
        if (private/'cancel').exists():raise OpsError('cancelled',409)
        if request['target']:
            current=[row for row in management.targets() if row['id']==request['target']['id']]
            if len(current)!=1 or current[0]['config_version']!=request['target']['config_version']:raise OpsError('configuration_changed',409)
            if request.get('automatic'):
                row=next((row for row in management.files() if row['name']==request['target']['auth_ref']),{})
                full=management.auth(request['target']['auth_ref'])
                automatic(row|{'note':full.get('note'),'plan_type':full.get('plan_type')},time.time())
    with (private/'browser.log').open('a') as log,contextlib.redirect_stdout(log),contextlib.redirect_stderr(log):
        try:
            management=Management(request['origin'],request['management_key_file'])
            guard()
            if sys.platform.startswith('linux'):
                read_fd,write_fd=os.pipe()
                display=subprocess.Popen(['Xvfb','-displayfd',str(write_fd),'-screen','0','1440x1000x24','-nolisten','tcp'],pass_fds=(write_fd,),stdout=log,stderr=log)
                os.close(write_fd)
                with os.fdopen(read_fd) as stream:number=stream.readline().strip()
                if not number.isdigit():raise OpsError('failed')
                os.environ['DISPLAY']=':'+number
                (private/'display').write_text(':'+number)
            os.chdir(request['login_project']);sys.path.insert(0,request['login_project'])
            os.environ.update(CODEX_AUTH_URL_SOURCE='cpa',CODEX_OAUTH_DRIVER='cloak',CPA_MANAGEMENT_URL=request['origin'],CPA_MANAGEMENT_KEY=management.key,
                CPA_SAVE_CALLBACK_RECEIPT='false',CLOAK_USE_PROXY='false',CLOAK_HEADLESS='false',CLOAK_USER_DATA_DIR='',CLOAK_KEEP_BROWSER_OPEN='false',CPA_CALLBACK_SUBMIT_RETRIES='2')
            logging.basicConfig(stream=log,level=logging.INFO,force=True)
            import core.codex_oauth as oauth
            import core.roxy_codex_oauth as browser
            import core.roxy_registration as registration
            import config.cloakbrowser as cloak_config
            if cloak_config.CLOAK_USE_PROXY:raise OpsError('configuration_changed')
            oauth.dedupe_cpa_auth_files_for_email=lambda *args,**kwargs:[]
            original_request=oauth._cpa_request_json
            def api(method,path,body=None):
                guard()
                if path.startswith('/v0/management/codex-auth-url'):
                    query={'provider':'codex','ops_email':request['email'],'ops_job':request['job_id']}
                    if request['target']:
                        query.update(ops_target=request['target']['auth_ref'],ops_version=request['target']['config_version'],ops_automatic='true' if request.get('automatic') else 'false')
                    return management.api('GET','/v8/management/oauth/auth-url?'+urllib.parse.urlencode(query))
                if path=='/v0/management/oauth-callback':
                    emit('callback');return management.api(method,'/v8/management/oauth/callback',body)
                if path.startswith('/v0/management/get-auth-status'):
                    return management.api(method,path.replace('/v0/management/get-auth-status','/v8/management/oauth/status',1),body)
                # Other reads use the existing acquisition adapter. Credential deletion
                # is disabled above and is never an allowed compatibility operation.
                if method!='GET':raise OpsError('invalid_input')
                return original_request(method,path,body)
            oauth._cpa_request_json=api
            def password(address):
                guard();emit('password')
                return request['credentials']['password'] if address==request['email'] else ''
            def totp(address):
                guard();emit('mfa')
                return request['credentials']['totp'] if address==request['email'] else ''
            oauth._account_registration_password=password;oauth._account_totp_secret=totp
            original_finish=browser._finish_consent_workspace
            original_click=browser._click_if_present
            finishing=False
            def inspect_workspace(driver):
                guard();emit('workspace')
                if not request['target']:return
                if urllib.parse.urlsplit(str(driver.current_url)).hostname!='auth.openai.com':return
                controls=driver.execute_script('return {workspace:/select (?:a |your )?workspace|choose (?:a |your )?workspace|选择.*工作区/i.test(document.body.innerText),values:Array.from(document.querySelectorAll(\'input[type="radio"]\')).map(x=>x.value)}')
                if not isinstance(controls,dict):raise OpsError('workspace_mismatch')
                if controls.get('workspace') or controls.get('values'):
                    workspace=request['target']['workspace']
                    if workspace not in controls.get('values',[]):raise OpsError('workspace_mismatch')
                    driver.page.locator('input[type="radio"][value='+json.dumps(workspace)+']').check(force=True)
            def finish(driver):
                nonlocal finishing
                finishing=True;inspect_workspace(driver);return original_finish(driver)
            def click(driver,*args,**kwargs):
                if finishing:inspect_workspace(driver)
                return original_click(driver,*args,**kwargs)
            browser._finish_consent_workspace=finish;browser._click_if_present=click
            original_challenge=registration._cloudflare_challenge_state
            class Clock:
                paused=0
                def time(self):return time.time()-self.paused
                def monotonic(self):return time.monotonic()-self.paused
                def __getattr__(self,name):return getattr(time,name)
            clock=Clock();browser.time=registration.time=clock
            def challenge(driver):
                state=original_challenge(driver)
                if state.get('present'):
                    started=time.monotonic();emit('login_email','human_required');driver.page.bring_to_front()
                    while state.get('present'):
                        guard();time.sleep(2);state=original_challenge(driver)
                    clock.paused+=time.monotonic()-started;emit('login_email')
                return state
            registration._cloudflare_challenge_state=challenge
            emit('login_email')
            reply=oauth.run_codex_oauth(request['email'],proxy='',force=True)
            if not reply.get('ok'):guard();raise OpsError('failed')
            emit('persist_verify','success')
        except OpsError as exc:emit('persist_verify',exc.reason)
        except Exception:
            import traceback
            traceback.print_exc(file=log)
            emit('persist_verify','failed')
        finally:
            if display:
                display.terminate()
                try:display.wait(timeout=10)
                except subprocess.TimeoutExpired:display.kill();display.wait()


if __name__=='__main__':main()
