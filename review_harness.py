"""Local read-path measurement runner; requires slow_proxy.py beside this file.

Usage: python3 review_harness.py /absolute/path/to/weed [session_MiB] [disk] [sse|sseonly]
SSE runs additionally require boto3. All data and configuration are temporary.
Use only when the checked local ports are available. No existing service is used.
"""
import subprocess, tempfile, time, urllib.request, os, json, pathlib, sys
root = tempfile.mkdtemp(prefix='seaweed-review-')
print(root, flush=True)
bin = os.path.abspath(sys.argv[1])
proxy = str(pathlib.Path(__file__).resolve().with_name('slow_proxy.py'))
session = sys.argv[2] if len(sys.argv) > 2 else '64'
disk = 'disk' in sys.argv[3:]
sse = 'sse' in sys.argv[3:] or 'sseonly' in sys.argv[3:]
sseonly = 'sseonly' in sys.argv[3:]
ps = []
import socket
for port in [22421, 32421, 22422, 32422, 22423, 32423, 22424, 32424]:
    with socket.socket() as sock:
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind(('127.0.0.1', port))

def launch(name, args):
    f = open(root + '/' + name + '.log', 'w')
    p = subprocess.Popen(args, cwd=root, stdout=f, stderr=f, env={**os.environ, **({'WEED_S3_SSE_KEY': 'isolated-review-fixture-only'} if name == 's3' and sse else {})})
    ps.append(p)

def req(url, data=None, method=None, headers={}):
    return urllib.request.urlopen(urllib.request.Request(url, data=data, method=method, headers=headers), timeout=180)

def wait(url):
    for _ in range(100):
        try:
            req(url).close()
            return
        except Exception:
            time.sleep(0.2)
    raise RuntimeError(url)
try:
    for d in ['master', 'volume', 'filer', 'cache']:
        os.mkdir(root + '/' + d)
    launch('master', [bin, 'master', '-ip=127.0.0.1', '-port=22421', '-mdir=' + root + '/master', '-volumeSizeLimitMB=100', '-telemetry=false'])
    wait('http://127.0.0.1:22421/cluster/status')
    launch('proxy', ['python3', proxy, '22423', '22423', '1', '12', '127.0.0.2'])
    launch('grpcproxy', ['python3', proxy, '32423', '32423', '1000', '1000', '127.0.0.2'])
    launch('volume', [bin, 'volume', '-ip=127.0.0.1', '-ip.bind=127.0.0.2', '-port=22423', '-publicUrl=127.0.0.1:22423', '-master=127.0.0.1:22421', '-dir=' + root + '/volume', '-max=10', '-minFreeSpacePercent=0'])
    launch('filer', [bin, 'filer', '-ip=127.0.0.1', '-port=22422', '-master=127.0.0.1:22421'])
    wait('http://127.0.0.1:22422/')
    launch('s3', [bin, 's3', '-ip.bind=127.0.0.1', '-port=22424', '-port.iceberg=0', '-port.lance=0', '-filer=127.0.0.1:22422', '-readerCacheSizeMB=512', '-sessionReaderCacheSizeMB=' + session] + (['-cacheDir=' + root + '/cache', '-cacheCapacityMB=2048', '-wholeCacheCapacityMB=51200'] if disk else []))
    wait('http://127.0.0.1:22424/')
    time.sleep(3)
    with open(root + '/blob', 'wb') as f:
        f.write(os.urandom(16 << 20))
    subprocess.run(['curl', '--max-time', '90', '-fsS', '-F', 'file=@' + root + '/blob', 'http://127.0.0.1:22422/buckets/review/blob?maxMB=2'], check=True, stdout=subprocess.DEVNULL)

    def measure(name, url, headers={}, expected=None):
        time.sleep(5.2)

        def count():
            lines = open(root + '/proxy.log').read().splitlines()
            return int(lines[-1].split()[-1]) if lines else 0
        before = count()
        start = time.monotonic()
        with req(url, headers=headers) as r:
            data = r.read()
        elapsed = time.monotonic() - start
        if expected is not None and data != expected:
            raise AssertionError(name + ' payload mismatch')
        time.sleep(5.2)
        after = count()
        print(json.dumps(dict(name=name, bytes=len(data), proxy_bytes=after - before, seconds=round(elapsed, 3))), flush=True)
    if not sseonly:
        measure('s3 whole', 'http://127.0.0.1:22424/review/blob')
        measure('s3 open range', 'http://127.0.0.1:22424/review/blob', {'Range': 'bytes=1048576-'})
        measure('s3 small range', 'http://127.0.0.1:22424/review/blob', {'Range': 'bytes=1024-33791'})
        if disk:
            measure('s3 warm disk', 'http://127.0.0.1:22424/review/blob')
        measure('filer whole', 'http://127.0.0.1:22422/buckets/review/blob')
    if sse:
        import boto3, base64, hashlib
        from botocore.config import Config
        from botocore import UNSIGNED
        client = boto3.client('s3', endpoint_url='http://127.0.0.1:22424', config=Config(signature_version=UNSIGNED), region_name='us-east-1')
        payload = os.urandom(8 << 20)
        key = base64.b64encode(b'k' * 32).decode()
        md5 = base64.b64encode(hashlib.md5(b'k' * 32).digest()).decode()
        for kind, kwargs, headers in [('plain', {}, {}), ('ssec', dict(SSECustomerAlgorithm='AES256', SSECustomerKey=key, SSECustomerKeyMD5=md5), {'x-amz-server-side-encryption-customer-algorithm': 'AES256', 'x-amz-server-side-encryption-customer-key': key, 'x-amz-server-side-encryption-customer-key-MD5': md5}), ('sses3', dict(ServerSideEncryption='AES256'), {})]:
            if sseonly and kind != 'sses3':
                continue
            for multipart in [False, True]:
                name = kind + ('-multipart' if multipart else '-single')
                if multipart:
                    uid = client.create_multipart_upload(Bucket='review', Key=name, **kwargs)['UploadId']
                    parts = []
                    for i, body in enumerate([payload[:5 << 20], payload[5 << 20:]], 1):
                        part = client.upload_part(Bucket='review', Key=name, UploadId=uid, PartNumber=i, Body=body, **kwargs if kind == 'ssec' else {})
                        parts.append(dict(PartNumber=i, ETag=part['ETag']))
                    client.complete_multipart_upload(Bucket='review', Key=name, UploadId=uid, MultipartUpload={'Parts': parts})
                else:
                    client.put_object(Bucket='review', Key=name, Body=payload, **kwargs)
                measure(name, 'http://127.0.0.1:22424/review/' + name, headers, payload)
finally:
    for p in reversed(ps):
        p.terminate()
    for p in ps:
        try:
            p.wait(timeout=10)
        except subprocess.TimeoutExpired:
            p.kill()
