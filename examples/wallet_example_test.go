package examples

import (
	"fmt"
	"log"
	"testing"

	"github.com/nknorg/nkn/common"
	"github.com/nknorg/nkn/vault"

	. "github.com/nknorg/nkn-sdk-go"
)

func TestWallet(t *testing.T) {
	err := func () error {
		Init()

		privateKey, _ := common.HexStringToBytes("039e481266e5a05168c1d834a94db512dbc235877f150c5a3cc1e3903672c67352dff44c21790d9edef7a7e3fc9bd7254359246d0ae605a3c97e71aad83d6b0d")
		account, err := vault.NewAccountWithPrivatekey(privateKey)
		if err != nil {
			return err
		}

		w := NewWalletSDK(account)

		// Query asset balance for this wallet
		balance, err := w.Balance()
		if err != nil {
			return err
		}
		log.Println("asset balance for this wallet is:", balance.String())

		// Transfer asset to some address
		// This call will fail because a new account has no balance
		address, err := account.ProgramHash.ToAddress()
		if err != nil {
			return err
		}
		txid, err := w.Transfer(address, "100")
		if err != nil {
			return err
		}
		log.Println("success:", txid)

		//// Register name for this wallet
		//txid, err = w.RegisterName("somename")
		//if err != nil {
		//	return err
		//}
		//log.Println("success:", txid)
		//
		//// Delete name for this wallet
		//// This call will fail because a new account has no name
		//txid, err = w.DeleteName("somename")
		//if err != nil {
		//	return err
		//}
		//log.Println("success:", txid)

		// Subscribe to bucket 0 of specified topic for this wallet for next 10 blocks
		txid, err = w.Subscribe("identifier", "topic", 0, 10, "meta")
		if err != nil {
			return err
		}
		log.Println("success:", txid)

		// Open nano pay channel to specified address
		np, err := w.NewNanoPay(address)
		if err != nil {
			return err
		}
		// Send 100 NKN into channel with claim available for next 100 blocks and channel open for next 200 blocks
		txid, err = np.Send("100", 100, 200)
		if err != nil {
			return err
		}
		log.Println("success:", txid)

		return nil
	}()
	if err != nil {
		fmt.Println(err)
	}
}