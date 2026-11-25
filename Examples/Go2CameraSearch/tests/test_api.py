from types import SimpleNamespace
import threading
from fastapi.testclient import TestClient
import app
from test_search import Twin
from controller import Controller


def client():
    c = Controller(Twin(),lambda:100.)
    app.app.state.runtime = SimpleNamespace(controller=c,image_lock=threading.Lock(),search_generation=0,tracker=None)
    return TestClient(app.app),c


def test_lan_dashboard_has_no_login_and_rejects_cross_site_commands():
    web,c=client()
    assert web.get('/').status_code==200
    assert web.get('/health').json()['app']=='go2-camera-search'
    headers={'Origin':'https://elsewhere.example','X-Operator':'b'*20}
    assert web.post('/api/stop',json={},headers=headers).status_code==403
    assert not c.twin.calls
    headers={'Origin':'http://testserver','X-Operator':'b'*20}
    assert web.post('/api/stop',json={},headers=headers).status_code==200


def test_unknown_commands_are_not_forwarded():
    web,c=client()
    headers={'X-Operator':'b'*20}
    assert web.post('/api/floor',json={'Enabled':True},headers=headers).status_code==404
    assert not c.twin.calls


def test_ambiguous_start_failure_attempts_stop():
    web,c=client()
    c.owner='b'*20
    c.twin.handle='handle'
    def ambiguous_start(owner):
        c.twin.snapshot['Moving']=True
        raise RuntimeError('connection lost after acceptance')
    c.start=ambiguous_start
    response=web.post('/api/start',json={},headers={'X-Operator':c.owner})
    assert response.status_code==503
    assert not c.twin.snapshot['Moving']
    assert any(op=='stop' for op,_ in c.twin.calls)


def test_camera_marking_requires_fresh_video():
    web,c=client()
    r=app.app.state.runtime
    r.image_at=0
    response=web.post('/api/camera-robot',json={'front':[320,240],'rear':[240,240]},headers={'X-Operator':'b'*20})
    assert response.status_code==422
    assert not c.twin.calls
