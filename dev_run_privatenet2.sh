#!/bin/sh

#   __        ___    _   _  ____ _           _       ____             
#   \ \      / / \  | \ | |/ ___| |__   __ _(_)_ __ |  _ \  _____   __
#    \ \ /\ / / _ \ |  \| | |   | '_ \ / _` | | '_ \| | | |/ _ \ \ / /
#     \ V  V / ___ \| |\  | |___| | | | (_| | | | | | |_| |  __/\ V / 
#      \_/\_/_/   \_\_| \_|\____|_| |_|\__,_|_|_| |_|____/ \___| \_/  
#                                                                     
# Run this file from the go-wanchain directory with ./dev_run_privatenet.sh
# First build gwan with make gwan
#
DATADIR=./data_privatenet2
ACCOUNT1=0x68489694189aa9081567dfc6d74a08c0c21d92c6
ACCOUNT2=0x184bfe537380d650533846c8c7e2a80d75acee63
KEYSTORE1=./accounts/keystore/${ACCOUNT1}
KEYSTORE2=./accounts/keystore/${ACCOUNT2}

# Cleanup the datadir
CLEANUP=true

NETWORKID='--networkid 99'

# Perform cleanup
if [ -d $DATADIR ]
then
  if [ "$CLEANUP" == "true" ]
  then
    rm -rf $DATADIR
    # Initialize chain
    ./build/bin/gwan ${NETWORKID} --etherbase "${ACCOUNT1}" --nat none --verbosity 4 \
      --datadir $DATADIR --identity LocalTestNode2 init ./core/genesis_privatenet.json
  fi
else
  mkdir $DATADIR
fi

if [ ! -d $DATADIR/keystore ]
then
  mkdir -p $DATADIR/keystore
fi

echo "password1" > ./passwd.txt
echo "password1" >> ./passwd.txt
cp $KEYSTORE1 $DATADIR/keystore
cp $KEYSTORE2 $DATADIR/keystore

PORT=17719
RPCPORT=8547

./build/bin/gwan ${NETWORKID} --etherbase "${ACCOUNT1}" --nat none --verbosity 3 --gasprice '200000' --datadir $DATADIR  \
    --unlock "${ACCOUNT1},${ACCOUNT2}" --password ./passwd.txt \
    --port ${PORT} --mine --minerthreads 1 \
    --maxpeers 5 --nodekey ./bootnode/privatenet2 \
    --rpc --rpcaddr 0.0.0.0 --rpcport ${RPCPORT} --rpcapi "eth,personal,net,admin,wan" --rpccorsdomain '*' \
    --bootnodes "enode://9c6d6f351a3ede10ed994f7f6b754b391745bba7677b74063ff1c58597ad52095df8e95f736d42033eee568dfa94c5a7689a9b83cc33bf919ff6763ae7f46f8d@127.0.0.1:17718" \
    --keystore $DATADIR/keystore \
    --identity LocalTestNode2