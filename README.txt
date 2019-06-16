GetTrainsPubSub is deployed as a Cloud Function which reacts to the /trains topic.
It saves the data as TrainDetails entities in Cloud Datastore.

Cloud scheduler triggers requests to the trains topic.
Relevant trains: 319-S01301 658-S06725 451-S01301