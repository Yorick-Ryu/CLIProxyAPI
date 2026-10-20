"""Fixed-origin v8 adapter; tokens are read in memory only and never returned to the UI."""
import json
import time
import urllib.error
import urllib.parse
import urllib.request
from .security import OpsError,private_file


class Management:
    def __init__(self,origin,key_file):
        parsed=urllib.parse.urlsplit(origin)
        if parsed.scheme!='http' or parsed.hostname not in ('127.0.0.1','localhost') or parsed.username or parsed.path or parsed.query:raise OpsError('invalid_input')
        self.origin=origin;self.key=private_file(key_file).decode().strip()
    def api(self,method,path,body=None):
        if not path.startswith('/v8/management/') and not path.startswith('/v0/management/plugins/cpa-codex-candy-eval/'):raise OpsError('invalid_input')
        request=urllib.request.Request(self.origin+path,method=method,data=None if body is None else json.dumps(body).encode(),headers={'Authorization':'Bearer '+self.key,'Content-Type':'application/json'})
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self,*args,**kwargs):return None
        try:
            with urllib.request.build_opener(NoRedirect).open(request,timeout=45) as response:
                raw=response.read(4*1024*1024+1)
                if len(raw)>4*1024*1024:raise OpsError('unavailable',503)
                return json.loads(raw)
        except urllib.error.HTTPError as exc:
            if exc.code==409:raise OpsError('configuration_changed',409) from None
            raise OpsError('unavailable',503) from None
        except (urllib.error.URLError,ValueError):raise OpsError('unavailable',503) from None
    def targets(self):
        result=self.api('GET','/v8/management/account-ops/targets')
        if result.get('guarded_oauth') is not True or result.get('commit_guard_protocol')!=2:raise OpsError('unavailable',503)
        return result.get('items',[])
    def files(self):return self.api('GET','/v8/management/credentials').get('files',[])
    def auth(self,name):return self.api('GET','/v8/management/credentials/download?name='+urllib.parse.quote(name,safe=''))
    def patch(self,target,fields):
        result=self.api('POST','/v8/management/account-ops/targets/'+target['id']+'/preferences',{'config_version':target['config_version'],'fields':fields})
        if result.get('verified') is not True:raise OpsError('configuration_changed',409)
        return result['config_version']
    def candy(self,name,check,wait=None):
        wait=wait or time.sleep
        route='/v0/management/plugins/cpa-codex-candy-eval'
        def entry():
            rows=self.api('GET',route+'/state').get('auths',[])
            return next((row for row in rows if row.get('id')==name),{})
        prior=entry()
        if prior.get('running'):raise OpsError('locked',409)
        seen={json.dumps(result,sort_keys=True) for result in prior.get('results',[])}
        models=[row['id'] for row in self.api('GET','/v8/management/credentials/models?name='+urllib.parse.quote(name,safe='')).get('models',[])]
        if 'gpt-6.1-sol' not in models:return False
        self.api('POST',route+'/run',{'auth_ids':[name],'model':'gpt-6.1-sol','effort':'low','runs':1,'model_catalog':{name:models}})
        for _ in range(60):
            check();after=entry()
            fresh=[result for result in after.get('results',[]) if json.dumps(result,sort_keys=True) not in seen and result.get('model')=='gpt-6.1-sol' and result.get('effort')=='low']
            if not after.get('running') and len(fresh)==1:
                import re
                result=fresh[0]
                if result.get('skipped') or re.search(r'(?<!\d)401(?!\d)|unauthori[sz]ed',str(result.get('error') or ''),re.I):return None
                return result.get('ok') is True and not result.get('error') and not result.get('skipped') and bool(re.search(r'(?<!\d)21(?!\d)',str(result.get('answer') or '')))
            wait(5)
        return False
