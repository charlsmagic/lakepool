package impl

import (
	pb "github.com/himanhimao/lakepool/backend/proto_sphere"
	"context"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/codes"
	"bytes"
	"strconv"
	"crypto/md5"
	"fmt"
	"sync"
	"github.com/himanhimao/lakepool/backend/sphere_server/internal/pkg/service"
	"github.com/himanhimao/lakepool/backend/sphere_server/internal/pkg/conf"
	"time"
	"math/big"
)

const (
	maxPid       = 4194303
	requestIdLen = 8
)

type SphereServer struct {
	Conf       *conf.SphereConfig
	Mgr        *service.Manager
	contextMap sync.Map
}

func (s *SphereServer) Register(ctx context.Context, in *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if len(in.SysInfo.Hostname) == 0 {
		st := status.New(codes.InvalidArgument, "Invalid argument - hostname")
		return nil, st.Err()
	}

	if in.SysInfo.Pid < 0 || in.SysInfo.Pid > maxPid {
		st := status.New(codes.InvalidArgument, fmt.Sprintf("%s:%d", "Invalid argument - pid", in.SysInfo.Pid))
		return nil, st.Err()
	}

	var coinService service.CoinService
	if coinService = s.Mgr.GetCoinService(in.Config.CoinType); coinService == nil {
		st := status.New(codes.InvalidArgument, fmt.Sprintf("%s:%s", "Invalid argument - coinType", in.Config.CoinType))
		return nil, st.Err()
	}

	if len(in.Config.PayoutAddress) == 0 || !coinService.IsValidAddress(in.Config.PayoutAddress, in.Config.UsedTestNet) {
		st := status.New(codes.InvalidArgument, "Invalid argument - payoutAddress")
		return nil, st.Err()
	}

	if len(in.Config.PoolTag) == 0 {
		st := status.New(codes.InvalidArgument, "Invalid argument - poolTag")
		return nil, st.Err()
	}

	registerId := s.calculateRegisterId(in.SysInfo)
	registerKey := service.GetRegisterKey(registerId)
	register := service.NewRegister().SetPayoutAddress(in.Config.PayoutAddress).SetPoolTag(in.Config.PoolTag).
		SetCoinType(in.Config.CoinType).SetUsedTestNet(in.Config.UsedTestNet).SetExtraNonce1Length(int(in.Config.ExtraNonce1Length)).
		SetExtraNonce2Length(int(in.Config.ExtraNonce2Length))

	if err := s.storeRegisterContext(registerKey, register); err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("Store context error: %s", err.Error()))
		return nil, st.Err()
	}
	return &pb.RegisterResponse{RegisterId: registerId}, nil
}

func (s *SphereServer) GetLatestStratumJob(ctx context.Context, in *pb.GetLatestStratumJobRequest) (*pb.GetLatestStratumJobResponse, error) {
	registerId := in.RegisterId
	if len(registerId) != requestIdLen {
		st := status.New(codes.InvalidArgument, "Abnormal - invalid argument requestId")
		return nil, st.Err()
	}
	var err error
	var register *service.Register
	var coinService service.CoinService
	var cacheService service.CacheService

	registerKey := service.GetRegisterKey(registerId)
	register, err = s.fetchRegisterContext(registerKey)

	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}

	if register == nil || !register.IsValid() {
		st := status.New(codes.InvalidArgument, "Abnormal - unknown requestId")
		return nil, st.Err()
	}

	coinService = s.Mgr.GetCoinService(register.GetCoinType());
	if coinService == nil {
		st := status.New(codes.Internal, "Abnormal - coin service")
		return nil, st.Err()
	}

	stratumJobPart, jobTransactions, err := coinService.GetLatestStratumJob(registerId, register)
	if err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("%s : %s", "Abnormal - get stratum job ", err))
		return nil, st.Err()
	}

	cacheService = s.Mgr.GetCacheService()
	jobKey := service.GetJobKey(registerId, stratumJobPart.GetMeta().GetHeight(), stratumJobPart.GetMeta().GetCurTimeTs())
	jobCacheExpireTs := s.Conf.JobCacheExpireTs
	if cacheService == nil {
		st := status.New(codes.Internal, "Abnormal - cache service")
		return nil, st.Err()
	}

	err = cacheService.SetBlockTransactions(jobKey, int(jobCacheExpireTs), jobTransactions)
	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}

	return &pb.GetLatestStratumJobResponse{Job: loadPBStratumJob(stratumJobPart)}, nil
}

func (s *SphereServer) SubmitShare(ctx context.Context, in *pb.SubmitShareRequest) (*pb.SubmitShareResponse, error) {
	registerId := in.RegisterId
	if len(registerId) != requestIdLen {
		st := status.New(codes.InvalidArgument, "Abnormal - invalid argument requestId")
		return nil, st.Err()
	}
	var err error
	var register *service.Register
	var coinService service.CoinService
	var cacheService service.CacheService

	registerKey := service.GetRegisterKey(registerId)
	register, err = s.fetchRegisterContext(registerKey)

	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}

	if register == nil || !register.IsValid() {
		st := status.New(codes.InvalidArgument, "Abnormal - unknown requestId")
		return nil, st.Err()
	}

	coinService = s.Mgr.GetCoinService(register.GetCoinType());
	stratumShare := in.Share
	jobKey := service.GetJobKey(registerId, stratumShare.Meta.Height, stratumShare.Meta.CurTimeTs)
	if coinService == nil {
		st := status.New(codes.Internal, "Abnormal - coin service")
		return nil, st.Err()
	}

	cacheService = s.Mgr.GetCacheService()
	if cacheService == nil {
		st := status.New(codes.Internal, "Abnormal - cache service")
		return nil, st.Err()
	}

	transactions, err := cacheService.GetBlockTransactions(jobKey)
	if err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("Abnormal - get block transactions:%s", err.Error()))
		return nil, st.Err()
	}

	if transactions == nil {
		return &pb.SubmitShareResponse{Result: &pb.SubmitShareResult{State: pb.StratumShareState_ERR_JOB_NOT_FOUND}}, nil
	}

	blockHeaderPart, coinBasePart := loadServiceParts(in.Share)
	block, err := coinService.MakeBlock(blockHeaderPart, coinBasePart, transactions)
	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}

	targetDifficulty := new(big.Int).SetUint64(in.Difficulty)
	isSolveHash, err := coinService.IsSolveHash(block.GetHash(), targetDifficulty)

	if err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("Abnormal - is slove hash:%s", err.Error()))
		return nil, st.Err()
	}

	if !isSolveHash {
		return &pb.SubmitShareResponse{Result: &pb.SubmitShareResult{State: pb.StratumShareState_ERR_LOW_DIFFICULTY_SHARE}}, nil
	}

	//duplicate Check
	shareKey := service.GetShareKey(registerId, in.Share.Meta.Height)
	isExistShare, err := cacheService.ExistShareHash(shareKey, block.GetHash())
	if err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("Abnormal - is exist share:%s", err.Error()))
		return nil, st.Err()
	}

	if isExistShare {
		return &pb.SubmitShareResponse{Result: &pb.SubmitShareResult{State: pb.StratumShareState_ERR_DUPLICATE_SHARE}}, nil
	}

	netTargetDifficulty, err := coinService.GetTargetDifficulty(blockHeaderPart.GetNBits())
	if err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("Abnormal - get target difficulty:%s", err.Error()))
		return nil, st.Err()
	}

	isSubmitHash, err := coinService.IsSolveHash(block.GetHash(), netTargetDifficulty)
	if err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("Abnormal - is submit hash:%s", err.Error()))
		return nil, st.Err()
	}

	var state pb.StratumShareState
	var submitState bool
	if isSubmitHash {
		submitState, err = coinService.SubmitBlock(block.GetData())
		if err != nil {
			st := status.New(codes.Internal, fmt.Sprintf("Abnormal - submit share :%s", err.Error()))
			return nil, st.Err()
		}
	}

	if submitState {
		state = pb.StratumShareState_SUC_SUBMIT_BLOCK
	} else {
		state = pb.StratumShareState_ERR_SUBMIT_BLOCK
	}
	shareComputePower, err := coinService.CalculateShareComputePower(targetDifficulty)
	if err != nil {
		st := status.New(codes.Internal, fmt.Sprintf("Abnormal - calculate share compute:%s", err.Error()))
		return nil, st.Err()
	}

	return &pb.SubmitShareResponse{Result: &pb.SubmitShareResult{State: state, Hash: block.GetHash(), ComputePower: float64(shareComputePower.Uint64())}}, nil
}

func (s *SphereServer) Subscribe(in *pb.GetLatestStratumJobRequest, stream pb.Sphere_SubscribeServer) error {
	registerId := in.RegisterId
	if len(registerId) != requestIdLen {
		st := status.New(codes.InvalidArgument, "Abnormal - invalid argument requestId")
		return st.Err()
	}
	var err error
	var register *service.Register
	var coinService service.CoinService
	var cacheService service.CacheService

	registerKey := service.GetRegisterKey(registerId)
	register, err = s.fetchRegisterContext(registerKey)

	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return st.Err()
	}

	if register == nil || !register.IsValid() {
		st := status.New(codes.InvalidArgument, "Abnormal - unknown requestId")
		return st.Err()
	}

	coinService = s.Mgr.GetCoinService(register.GetCoinType());
	if coinService == nil {
		st := status.New(codes.Internal, "Abnormal - coin service")
		return st.Err()
	}

	cacheService = s.Mgr.GetCacheService()
	if cacheService == nil {
		st := status.New(codes.Internal, "Abnormal - cache service")
		return st.Err()
	}

	pullGBTInterval := s.Conf.PullGBTInterval
	notifyInterval := s.Conf.NotifyInterval
	notifyTicker := time.NewTicker(time.Second * notifyInterval)
	pullGBTTicker := time.NewTicker(time.Millisecond * pullGBTInterval)

	var stratumJobPart *service.StratumJobPart
	var blockTransactions []*service.BlockTransactionPart
	var tmpJobPart *service.StratumJobPart
	var tmpBlockTransactionParts []*service.BlockTransactionPart
	jobCacheExpireTs := int(s.Conf.JobCacheExpireTs)

	for {
		select {
		case <-stream.Context().Done():
			goto Out
		case <-notifyTicker.C:
			if stratumJobPart != nil {
				jobKey := service.GetJobKey(registerId, stratumJobPart.GetMeta().GetHeight(), stratumJobPart.GetMeta().GetCurTimeTs())
				err = cacheService.SetBlockTransactions(jobKey, int(jobCacheExpireTs), blockTransactions)
				if err != nil {
					st := status.New(codes.Internal, err.Error())
					return st.Err()
				}
				stream.Send(&pb.GetLatestStratumJobResponse{Job: loadPBStratumJob(tmpJobPart)})
			}
		case <-pullGBTTicker.C:
			tmpJobPart, tmpBlockTransactionParts, err = coinService.GetLatestStratumJob(registerId, register)
			if err != nil {
				break
			}
			if stratumJobPart != nil {
				if stratumJobPart.GetMeta().GetHeight() != tmpJobPart.GetMeta().GetHeight() {
					jobKey := service.GetJobKey(registerId, tmpJobPart.GetMeta().GetHeight(), tmpJobPart.GetMeta().GetCurTimeTs())
					err = cacheService.SetBlockTransactions(jobKey, jobCacheExpireTs, tmpBlockTransactionParts)
					if err != nil {
						st := status.New(codes.Internal, err.Error())
						return st.Err()
					}
					stream.Send(&pb.GetLatestStratumJobResponse{Job: loadPBStratumJob(tmpJobPart)})
					break
				}
			}
			stratumJobPart = tmpJobPart
			blockTransactions = tmpBlockTransactionParts
		}
	}
Out:
	return nil
}

func (s *SphereServer) ClearShareHistory(ctx context.Context, in *pb.ClearShareHistoryRequest) (*pb.ClearShareHistoryResponse, error) {
	registerId := in.RegisterId
	if len(registerId) != requestIdLen {
		st := status.New(codes.InvalidArgument, "Abnormal - invalid argument requestId")
		return nil, st.Err()
	}
	var err error
	var register *service.Register
	var cacheService service.CacheService

	registerKey := service.GetRegisterKey(registerId)
	register, err = s.fetchRegisterContext(registerKey)

	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}

	if register == nil || !register.IsValid() {
		st := status.New(codes.InvalidArgument, "Abnormal - unknown requestId")
		return nil, st.Err()
	}

	cacheService = s.Mgr.GetCacheService()
	if cacheService == nil {
		st := status.New(codes.Internal, "Abnormal - cache service")
		return nil, st.Err()
	}

	err = cacheService.ClearShareHistory(service.GetShareKey(registerId, in.Height))
	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}

	return &pb.ClearShareHistoryResponse{Result: true}, nil
}

func (s *SphereServer) UnRegister(ctx context.Context, in *pb.UnRegisterRequest, ) (*pb.UnRegisterResponse, error) {
	registerId := in.RegisterId
	if len(registerId) != requestIdLen {
		st := status.New(codes.InvalidArgument, "Abnormal - Invalid argument requestId")
		return nil, st.Err()
	}
	var err error
	var register *service.Register
	var cacheService service.CacheService

	registerKey := service.GetRegisterKey(registerId)
	register, err = s.fetchRegisterContext(registerKey)

	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}

	if register == nil || !register.IsValid() {
		st := status.New(codes.InvalidArgument, "Abnormal - unknown requestId")
		return nil, st.Err()
	}

	cacheService = s.Mgr.GetCacheService()
	if cacheService == nil {
		st := status.New(codes.Internal, "Abnormal - cache service")
		return nil, st.Err()
	}

	err = cacheService.DelRegisterContext(service.GetRegisterKey(registerId))
	if err != nil {
		st := status.New(codes.Internal, err.Error())
		return nil, st.Err()
	}
	s.contextMap.Store(registerKey, nil)
	return &pb.UnRegisterResponse{Result: true}, nil
}

func (s *SphereServer) calculateRegisterId(info *pb.SysInfo) string {
	buf := new(bytes.Buffer)
	buf.WriteString(info.Hostname)
	buf.WriteString(strconv.Itoa(int(info.Pid)))
	hash := md5.Sum(buf.Bytes())
	registerId := fmt.Sprintf("%x", hash)
	return registerId[3:11]
}

func (s *SphereServer) storeRegisterContext(registerKey service.RegisterKey, r *service.Register) error {
	s.contextMap.Store(registerKey, r)
	return s.Mgr.GetCacheService().SetRegisterContext(registerKey, r)
}

func (s *SphereServer) fetchRegisterContext(registerKey service.RegisterKey) (*service.Register, error) {
	if value, ok := s.contextMap.Load(registerKey); ok {
		return value.(*service.Register), nil
	}
	return s.Mgr.GetCacheService().GetRegisterContext(registerKey)
}

func loadPBStratumJob(job *service.StratumJobPart) *pb.StratumJob {
	pbStratumJob := new(pb.StratumJob)
	pbStratumJob.NBits = job.GetNBits()
	pbStratumJob.PrevHash = job.GetPrevHash()
	pbStratumJob.MerkleBranch = job.GetMerkleBranch()
	pbStratumJob.CoinBase1 = job.GetCoinBase1()
	pbStratumJob.CoinBase2 = job.GetCoinBase2()
	pbStratumJob.Version = job.GetVersion()

	pbStratumJobMeta := new(pb.StratumJobMeta)
	pbStratumJobMeta.CurTimeTs = job.GetMeta().GetCurTimeTs()
	pbStratumJobMeta.Height = job.GetMeta().GetHeight()
	pbStratumJobMeta.MinTimeTs = job.GetMeta().GetMinTimeTs()
	pbStratumJob.Meta = pbStratumJobMeta
	return pbStratumJob
}

func loadServiceParts(share *pb.StratumShare) (*service.BlockHeaderPart, *service.BlockCoinBasePart) {
	coinBasePart := service.NewBlockCoinBasePart()
	coinBasePart.SetCoinBase1(share.CoinBase1)
	coinBasePart.SetCoinBase2(share.CoinBase2)
	coinBasePart.SetExtraNonce1(share.ExtraNonce1)
	coinBasePart.SetExtraNonce2(share.ExtraNonce2)

	blockHeaderPart := service.NewBlockHeaderPart()
	blockHeaderPart.SetNTime(share.NTime)
	blockHeaderPart.SetNBits(share.NBits)
	blockHeaderPart.SetVersion(share.Version)
	blockHeaderPart.SetNonce(share.Nonce)
	blockHeaderPart.SetPrevHash(share.PrevHash)
	return blockHeaderPart, coinBasePart
}