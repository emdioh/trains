PYRUNTIME=--runtime python37
DEFAULT_ARGS=--runtime python37 --entry-point main --memory 128MB \
	--service-account datastoreuser@k-home-misc.iam.gserviceaccount.com \

GetTrain_http:
	cd ./GetTrain/http && gcloud functions deploy GetTrain $(DEFAULT_ARGS) \
	--trigger-http

GetTrain_pubsub:
	cd ./GetTrain/pubsub && gcloud functions deploy GetTrainPubSub  $(DEFAULT_ARGS) \
	--trigger-topic=trains

GetTrain: GetTrain_http GetTrain_pubsub

SCHEDULER_PARAMS=pubsub Trigger_train_get --schedule "30 18 * * *" --topic "trains" \
		--message-body-from-file=get_trains.cronjob.payload --time-zone="Europe/Rome"
CreateCronJob:
	gcloud scheduler jobs create $(SCHEDULER_PARAMS)

UpdateCronJob:
	gcloud scheduler jobs update $(SCHEDULER_PARAMS)

pubsub: GetTrain_pubsub
all: GetTrain

