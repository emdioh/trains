import datetime
import json
import sqlite3
import urllib.request

PROJECT = 'k-home-misc'

def get_train_data(train_no, departure_station):
    url = 'http://www.viaggiatreno.it/viaggiatrenonew/resteasy/viaggiatreno/andamentoTreno/{}/{}'.format(
        departure_station,train_no)
    req = urllib.request.Request(url)
    response = urllib.request.urlopen(req).read()
    content = json.loads(response.decode('utf-8'))
    return content

def convert_epoch(timestamp):
    return datetime.datetime.fromtimestamp(timestamp/1000)

def has_arrived(data):
    return convert_epoch(data['orarioArrivo']) < datetime.datetime.now()

def save_train(train_no, departure_station, data):
    from google.cloud import datastore
    datastore_client = datastore.Client(PROJECT)
    
    kind = 'TrainDetails'
    now = datetime.datetime.now()
    key = datastore_client.key(kind)
    task = datastore.Entity(key=key, exclude_from_indexes=['data'])
    #task = datastore.Entity(key='{}-{}-{}'.format(now.strftime('%Y%m%d'),train_no, departure_station))
    task['train_no'] = train_no
    task['departure_station'] = departure_station
    task['utc_timestamp'] = now.isoformat()
    task['ritardo'] = data['ritardo']
    task['data'] = json.dumps(data)
    
    datastore_client.put(task)
    return True
    
def get_train(request):
    """Responds to any HTTP request.
    Args:
        request (flask.Request): HTTP request object.
    Returns:
        The response text or any set of values that can be turned into a
        Response object using
        `make_response <http://flask.pocoo.org/docs/1.0/api/#flask.Flask.make_response>`.
    """
    request_json = request.get_json()
    train = None
    station = None

    if request.args:
        for k in request.args.keys():
            print("args - {}:{}".format(k,request.args[k]))
    if request_json:
        for k in request_json.keys():
            print("args - {}:{}".format(k,request_json[k]))

    if request.args and 'train' in request.args:
        train = request.args['train']
    if request.args and 'station' in request.args:
        station = request.args['station']
    if request_json and 'train' in request_json:
        train = request_json['train']
    if request_json and 'station' in request_json:
        station = request_json['station']
    if not train or not station:
        print ('train and station are parameters are required')
        return f'train and station parameters are required'
    print('Getting data for {}-{}'.format(train, station))
    data = get_train_data(train, station)

    if data:
        if save_train(train, station, data):
           return 'OK'

    from flask import abort
    return abort(500)


