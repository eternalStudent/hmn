#!/bin/bash

if [ $SERVICE_RESULT == "success" ]; then
	exit
fi

/home/hmn/hmn/adminmailer/adminmailer "[$1] Status changed" <<ERRMAIL
$(service "$1" status)
ERRMAIL
