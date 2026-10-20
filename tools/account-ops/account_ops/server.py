"""Authenticated HTTP over a filesystem-protected Unix socket, never a public port."""
import argparse
import fcntl
import hmac
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import socketserver
import signal
import threading
import urllib.parse
from .engine import Engine
from .legacy import Legacy
from .security import OpsError, Vault, private_file
from .service import Service
from .store import Store


class UnixHTTPServer(socketserver.ThreadingMixIn,socketserver.UnixStreamServer):
    daemon_threads=True


class Handler(BaseHTTPRequestHandler):
    protocol_version='HTTP/1.0'
    def log_message(self,*args):pass
    def handle_request(self):
        try:
            if not hmac.compare_digest(self.headers.get('Authorization',''),'Bearer '+self.server.secret):raise OpsError('unavailable',401)
            if self.headers.get('Transfer-Encoding'):raise OpsError('invalid_input')
            path=urllib.parse.urlsplit(self.path)
            if path.scheme or path.netloc or path.fragment or '%' in path.path or '..' in path.path or '\\' in path.path:raise OpsError('invalid_input')
            length=int(self.headers.get('Content-Length','0'))
            if not 0<=length<=300_000:raise OpsError('invalid_input',413)
            body={}
            if length:
                if self.headers.get_content_type()!='application/json':raise OpsError('invalid_input',415)
                body=json.loads(self.rfile.read(length))
                if not isinstance(body,dict):raise OpsError('invalid_input')
            query={k:v[0] for k,v in urllib.parse.parse_qs(path.query).items() if len(v)==1}
            status,result=self.server.service.dispatch(self.command,path.path,query,body,self.headers.get('Idempotency-Key'))
        except OpsError as exc:status,result=exc.status,{'error':exc.reason}
        except (ValueError,TypeError,UnicodeError):status,result=400,{'error':'invalid_input'}
        except Exception:status,result=500,{'error':'failed'}
        payload=json.dumps(result,ensure_ascii=False,allow_nan=False).encode()
        self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Cache-Control','no-store')
        self.send_header('X-Content-Type-Options','nosniff');self.send_header('Content-Length',str(len(payload)));self.end_headers()
        self.wfile.write(payload)
    do_GET=do_POST=do_PUT=do_PATCH=do_DELETE=handle_request


def serve(root,socket_path,key_path,token_path,paths=None,execution=None,instance="clouvider"):
    os.umask(0o077)
    root=Path(root)
    root.mkdir(mode=0o700,parents=True,exist_ok=True)
    with (root/'service.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        vault=Vault(key_path);secret=private_file(token_path).decode().strip()
        if len(secret)<32:raise OpsError('unavailable',503)
        store=Store(root,vault,instance=instance)
        adapter=None
        if execution:
            from .execution import Executor
            adapter=Executor(store,execution)
        engine=Engine(store,adapter);service=Service(store,Legacy(store,paths),adapter)
        if adapter:
            from .scheduling import Scheduler
            scheduler=Scheduler(store,adapter);engine.scheduler=scheduler;service.scheduler=scheduler
        # bind refuses an existing socket. Operators reconcile interrupted tasks and
        # verify ownership before removing a stale socket; startup never unlinks one.
        server=UnixHTTPServer(str(socket_path),Handler)
        os.chmod(socket_path,0o660)
        inode=Path(socket_path).stat().st_ino
        server.secret,server.service=secret,service
        engine.start()
        def stop_service(signum,frame):
            threading.Thread(target=server.shutdown,daemon=True).start()
        signal.signal(signal.SIGTERM,stop_service);signal.signal(signal.SIGINT,stop_service)
        try:server.serve_forever(poll_interval=.5)
        finally:
            engine.close();server.server_close();store.close()
            if Path(socket_path).exists() and Path(socket_path).stat().st_ino==inode:Path(socket_path).unlink()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config',required=True)
    options=parser.parse_args()
    config=json.loads(private_file(options.config))
    allowed={'root','socket','master_key_file','service_token_file','legacy_paths','execution','instance'}
    if set(config)-allowed:raise SystemExit('Invalid account operations configuration')
    serve(config['root'],config['socket'],config['master_key_file'],config['service_token_file'],config.get('legacy_paths'),config.get('execution'),config.get('instance','clouvider'))


if __name__=='__main__':main()
