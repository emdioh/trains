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
    
def get_train(data, context):
    """Responds to any HTTP request.
    Args:
         data (dict): The dictionary with data specific to this type of event.
         context (google.cloud.functions.Context): The Cloud Functions event
         metadata.
    """

    from flask import abort
    print("data: {}".format(json.dumps(data)))

    good = False
    for r in data:
        train = None
        station = None
        if 'train' in r:
            train = r['train']
        if 'station' in r:
            station = r['station']
        if not train or not station:
            print ('train and station are parameters are required')
            print ('Got train:{} station:{}'.format(train,station))
            continue
        print('Getting data for {}-{}'.format(train, station))
        train_data = get_train_data(train, station)

        if train_data:
            if save_train(train, station, train_data):
               good = True
            else:
               print("Failed saving train data")
        else:
            print("Failed retrieving train data")

    if good:
        print('OK')
 


